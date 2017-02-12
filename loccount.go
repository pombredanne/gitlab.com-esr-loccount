// SPDX-License-Identifier: BSD-2-Clause
package main

import "bytes"
import "bufio"
import "fmt"
import "flag"
import "io"
import "math"
import "os"
import "path/filepath"
import "regexp"
import "runtime"
import "sort"
import "strings"
import "log"
import "sync"
import "runtime/pprof"

const version float32 = 1.0

/*
How to add support for a language to this program:

All the language-specific information this program needs to know to do
its job is the syntax of comments and string literals.  Generally,
languages fall into one of the following groups:

* C-like: C is the prototype. This code recognizes them by file
  extension only.  These languages have two kinds of comment.  One is
  a block comment delimited by two distinct strings and the second is
  a winged comment introduced by a third string and terminated by
  newline.  The following bool signals whether newlines are permitted
  in strings.  You can add support simply by appending an initializer
  to the genericLanguages table; any entry with a nonempty comment
  leader invokes C-like parsing.

* Generic languages have only winged comments, usually led with #.
  This code recognizes them by file extension only.  You can append an
  initializer to the genericLanguages table specifying a name, an
  extension, and the winged-comment leader.  Any entry with empty
  commentleader and trailer strings gets generic parsing.

* Scripting languages have only winged comments, always led with #.
  This code recognizes them by file extension, or by looking for a
  hashbang line identifying the interpreter.  You can append an
  initializer to the scriptingLanguages table specifying a name, an
  extension, and a matching string to look for in a hashbang line.

* Pascal-likes use the (* *) block comment syntax.  This code
  recognizes them by file extension only.  You can append an
  initializer to the PascalLikes table specifying a name, an
  extension, and a boolean saying whether the language uses { } as
  additional pair of block comments.

* Fortran-likes use various start-of-line characters as comment
  leaders.  This code recognizes them by file extension only.  You can
  append an initializer to the fortranLikes table specifying a pair of
  (compiled) regular expressions; comments are recognized by matching
  the first and not the second.

You may add multiple entries with the same language name, but extensions
must be unique across all tables.
*/

// Following code swiped from Michael T. Jones's "walk" package.
// It's a parallelized implementation of tree-walking that's
// faster than the version in the system filepath library.

type VisitData struct {
	path string
	info os.FileInfo
}

// WalkFunc is the type of the function called for each file or directory
// visited by Walk. The path argument contains the argument to Walk as a
// prefix; that is, if Walk is called with "dir", which is a directory
// containing the file "a", the walk function will be called with argument
// "dir/a". The info argument is the os.FileInfo for the named path.
//
// If there was a problem walking to the file or directory named by path, the
// incoming error will describe the problem and the function can decide how
// to handle that error (and Walk will not descend into that directory). If
// an error is returned, processing stops. The sole exception is that if path
// is a directory and the function returns the special value SkipDir, the
// contents of the directory are skipped and processing continues as usual on
// the next file.
type WalkFunc func(path string, info os.FileInfo, err error) error

type WalkState struct {
	walkFn     WalkFunc
	v          chan VisitData // files to be processed
	active     sync.WaitGroup // number of files to process
	lock       sync.RWMutex
	firstError error // accessed using lock
}

func (ws *WalkState) terminated() bool {
	ws.lock.RLock()
	done := ws.firstError != nil
	ws.lock.RUnlock()
	return done
}

func (ws *WalkState) setTerminated(err error) {
	ws.lock.Lock()
	if ws.firstError == nil {
		ws.firstError = err
	}
	ws.lock.Unlock()
	return
}

func (ws *WalkState) visitChannel() {
	for file := range ws.v {
		ws.visitFile(file)
		ws.active.Add(-1)
	}
}

// readDirNames reads the directory named by dirname and returns
// a sorted list of directory entries.
func readDirNames(dirname string) ([]string, error) {
	f, err := os.Open(dirname)
	if err != nil {
		return nil, err
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return nil, err
	}
	sort.Strings(names) // omit sort to save 1-2%
	return names, nil
}

func (ws *WalkState) visitFile(file VisitData) {
	if ws.terminated() {
		return
	}

	err := ws.walkFn(file.path, file.info, nil)
	if err != nil {
		if !(file.info.IsDir() && err == filepath.SkipDir) {
			ws.setTerminated(err)
		}
		return
	}

	if !file.info.IsDir() {
		return
	}

	names, err := readDirNames(file.path)
	if err != nil {
		err = ws.walkFn(file.path, file.info, err)
		if err != nil {
			ws.setTerminated(err)
		}
		return
	}

	here := file.path
	for _, name := range names {
		file.path = filepath.Join(here, name)
		file.info, err = os.Lstat(file.path)
		if err != nil {
			err = ws.walkFn(file.path, file.info, err)
			if err != nil && (!file.info.IsDir() || err != filepath.SkipDir) {
				ws.setTerminated(err)
				return
			}
		} else {
			switch file.info.IsDir() {
			case true:
				ws.active.Add(1) // presume channel send will succeed
				select {
				case ws.v <- file:
					// push directory info to queue for concurrent traversal
				default:
					// undo increment when send fails and handle now
					ws.active.Add(-1)
					ws.visitFile(file)
				}
			case false:
				err = ws.walkFn(file.path, file.info, nil)
				if err != nil {
					ws.setTerminated(err)
					return
				}
			}
		}
	}
}

// Walk walks the file tree rooted at root, calling walkFn for each file or
// directory in the tree, including root. All errors that arise visiting files
// and directories are filtered by walkFn. The files are walked in a random
// order. Walk does not follow symbolic links.

func Walk(root string, walkFn WalkFunc) error {
	info, err := os.Lstat(root)
	if err != nil {
		return walkFn(root, nil, err)
	}

	ws := &WalkState{
		walkFn: walkFn,
		v:      make(chan VisitData, 1024),
	}
	defer close(ws.v)

	ws.active.Add(1)
	ws.v <- VisitData{root, info}

	walkers := 16
	for i := 0; i < walkers; i++ {
		go ws.visitChannel()
	}
	ws.active.Wait()

	return ws.firstError
}

// Swiped code ends here

type SourceStat struct {
	Path     string
	Language string
	SLOC     uint
}

var debug int
var exclusions []string
var pipeline chan SourceStat

// Data tables driving the recognition and counting of classes of languages.

type genericLanguage struct {
	name           string
	suffix         string
	commentleader  string
	commenttrailer string
	eolcomment     string
	multistring    string
	eolwarn        bool
	verifier       func(*countContext, string) bool
}

var genericLanguages []genericLanguage

type scriptingLanguage struct {
	name     string
	suffix   string
	hashbang string
	verifier func(*countContext, string) bool
}

var scriptingLanguages []scriptingLanguage

type pascalLike struct {
	name            string
	suffix          string
	bracketcomments bool
	verifier        func(*countContext, string) bool
}

var pascalLikes []pascalLike

const dt = "\"\"\""
const st = "'''"

var dtriple, striple, dtrailer, strailer, dlonely, slonely *regexp.Regexp

var podheader *regexp.Regexp

type fortranLike struct {
	name      string
	suffix    string
	comment   *regexp.Regexp
	nocomment *regexp.Regexp
}

var fortranLikes []fortranLike

var neverInterestingByPrefix []string
var neverInterestingByInfix []string
var neverInterestingBySuffix map[string]bool
var neverInterestingByBasename map[string]bool

var cHeaderPriority []string
var generated string

func init() {
	// For speed, try to put more common languages and extensions
	// earlier in this list.
	//
	// Verifiers are expensive, so try to put extensions that need them
	// after extensions that don't. But remember that you don't pay the
	// overhead for a verifier unless the extension matches.
	//
	// If you have mutiple entries for an extension, (a) all the entries
	// with verifiers should go first, and (b) there should be at most one
	// entry without a verifier (because any second and later ones will be
	// pre-empted by it).
	//
	// All entries for a given language should be in a contiguous span,
	// otherwise the primitive duplicate director in listLanguages will
	// be foiled.
	genericLanguages = []genericLanguage{
		/* C family */
		{"c", ".c", "/*", "*/", "//", "", true, nil},
		{"c-header", ".h", "/*", "*/", "//", "", true, nil},
		{"c-header", ".hpp", "/*", "*/", "//", "", true, nil},
		{"c-header", ".hxx", "/*", "*/", "//", "", true, nil},
		{"yacc", ".y", "/*", "*/", "//", "", true, nil},
		{"lex", ".l", "/*", "*/", "//", "", true, really_is_lex},
		{"c++", ".cpp", "/*", "*/", "//", "", true, nil},
		{"c++", ".cxx", "/*", "*/", "//", "", true, nil},
		{"c++", ".cc", "/*", "*/", "//", "", true, nil},
		{"java", ".java", "/*", "*/", "//", "", true, nil},
		{"javascript", ".js", "/*", "*/", "//", "", true, nil},
		{"obj-c", ".m", "/*", "*/", "//", "", true, really_is_objc},
		{"c#", ".cs", "/*", "*/", "//", "", true, nil},
		{"php", ".php", "/*", "*/", "//", "", true, nil},
		{"php3", ".php", "/*", "*/", "//", "", true, nil},
		{"php4", ".php", "/*", "*/", "//", "", true, nil},
		{"php5", ".php", "/*", "*/", "//", "", true, nil},
		{"php6", ".php", "/*", "*/", "//", "", true, nil},
		{"php7", ".php", "/*", "*/", "//", "", true, nil},
		{"go", ".go", "/*", "*/", "//", "`", true, nil},
		{"swift", ".swift", "/*", "*/", "//", "", true, nil},
		{"sql", ".sql", "/*", "*/", "--", "", false, nil},
		{"haskell", ".hs", "{-", "-}", "--", "", true, nil},
		{"pl/1", ".pl1", "/*", "*/", "", "", true, nil},
		/* everything else */
		{"asm", ".asm", "", "", ";", "", true, nil},
		{"asm", ".s", "", "", ";", "", true, nil},
		{"asm", ".S", "", "", ";", "", true, nil},
		{"ada", ".ada", "", "", "--", "", true, nil},
		{"ada", ".adb", "", "", "--", "", true, nil},
		{"ada", ".ads", "", "", "--", "", true, nil},
		{"ada", ".pad", "", "", "--", "", true, nil}, // Oracle Ada preprocessoer.
		{"css", ".css", "/*", "*/", "", "", true, nil},
		{"makefile", ".mk", "", "", "#", "", true, nil},
		{"makefile", "Makefile", "", "", "#", "", true, nil},
		{"makefile", "makefile", "", "", "#", "", true, nil},
		{"makefile", "Imakefile", "", "", "#", "", true, nil},
		{"m4", ".m4", "", "", "#", "", true, nil},
		{"lisp", ".lisp", "", "", ";", "", true, nil},
		{"lisp", ".lsp", "", "", ";", "", true, nil}, // XLISP
		{"lisp", ".cl", "", "", ";", "", true, nil},  // Common Lisp
		{"lisp", ".l", "", "", ";", "", true, nil},
		{"scheme", ".scm", "", "", ";", "", true, nil},
		{"elisp", ".el", "", "", ";", "", true, nil},    // Emacs Lisp
		{"clojure", ".clj", "", "", ";", "", true, nil}, // Clojure
		{"clojure", ".cljc", "", "", ";", "", true, nil},
		{"clojurescript", ".cljs", "", "", ";", "", true, nil},
		{"cobol", ".CBL", "", "", "*", "", true, nil},
		{"cobol", ".cbl", "", "", "*", "", true, nil},
		{"cobol", ".COB", "", "", "*", "", true, nil},
		{"cobol", ".cob", "", "", "*", "", true, nil},
		{"eiffel", ".e", "", "", "--", "", true, nil},
		{"sather", ".sa", "", "", "--", "", true, really_is_sather},
		{"lua", ".lua", "", "", "--", "", true, nil},
		{"clu", ".clu", "", "", "%", "", true, nil},
		{"rust", ".rs", "", "", "//", "", true, nil},
		{"rust", ".rlib", "", "", "//", "", true, nil},
		{"erlang", ".erl", "", "", "%", "", true, nil},
		//{"turing", ".t", "", "", "%", "", true, nil},
		{"d", ".d", "", "", "//", "", true, nil},
		{"occam", ".f", "", "", "//", "", true, really_is_occam},
		{"prolog", ".pl", "", "", "%", "", true, really_is_prolog},
		{"mumps", ".m", "", "", ";", "", true, nil},
		{"pop11", ".p", "", "", ";", "", true, really_is_pop11},
		// autoconf cruft
		{"autotools", "config.h.in", "/*", "*/", "//", "", true, nil},
		{"autotools", "autogen.sh", "", "", "#", "", true, nil},
		{"autotools", "configure.in", "", "", "#", "", true, nil},
		{"autotools", "Makefile.in", "", "", "#", "", true, nil},
		{"autotools", ".am", "", "", "#", "", true, nil},
		{"autotools", ".ac", "", "", "#", "", true, nil},
		{"autotools", ".mf", "", "", "#", "", true, nil},
		// Scons
		{"scons", "SConstruct", "", "", "#", "", true, nil},
	}

	var err error
	dtriple, err = regexp.Compile(dt + "." + dt)
	if err != nil {
		panic(err)
	}
	striple, err = regexp.Compile(st + "." + st)
	if err != nil {
		panic(err)
	}
	dlonely, err = regexp.Compile("^[ \t]*\"[^\"]+\"")
	if err != nil {
		panic(err)
	}
	slonely, err = regexp.Compile("^[ \t]*'[^']+'")
	if err != nil {
		panic(err)
	}
	strailer, err = regexp.Compile(".*" + st)
	if err != nil {
		panic(err)
	}
	dtrailer, err = regexp.Compile(".*" + dt)
	if err != nil {
		panic(err)
	}

	scriptingLanguages = []scriptingLanguage{
		{"tcl", ".tcl", "tcl", nil}, /* before sh, because tclsh */
		{"tcl", ".tcl", "wish", nil},
		{"csh", ".csh", "csh", nil},
		{"shell", ".sh", "sh", nil},
		{"ruby", ".rb", "ruby", nil},
		{"awk", ".awk", "awk", nil},
		{"sed", ".sed", "sed", nil},
		{"expect", ".exp", "expect", really_is_expect},
	}
	pascalLikes = []pascalLike{
		{"pascal", ".pas", true, nil},
		{"pascal", ".p", true, really_is_pascal},
		{"pascal", ".inc", true, really_is_pascal},
		{"modula3", ".i3", false, nil},
		{"modula3", ".m3", false, nil},
		{"modula3", ".ig", false, nil},
		{"modula3", ".mg", false, nil},
		{"ml", ".ml", false, nil},
		{"mli", ".ml", false, nil},
		{"mll", ".ml", false, nil},
		{"mly", ".ml", false, nil},
		{"oberon", ".mod", false, nil},
	}

	var ferr error
	f90comment, ferr := regexp.Compile("^([ \t]*!|[ \t]*$)")
	if ferr != nil {
		panic("unexpected failure while building f90 comment analyzer")
	}
	f90nocomment, ferr := regexp.Compile("^[ \t]*!(hpf|omp)[$]")
	if ferr != nil {
		panic("unexpected failure while building f90 no-comment analyzer")
	}
	f77comment, ferr := regexp.Compile("^([c*!]|[ \t]+!|[ \t]*$)")
	if ferr != nil {
		panic("unexpected failure while building f77 comment analyzer")
	}
	f77nocomment, ferr := regexp.Compile("^[c*!](hpf|omp)[$]")
	if ferr != nil {
		panic("unexpected failure while building f77 nocomment analyzer")
	}
	fortranLikes = []fortranLike{
		{"fortran90", ".f90", f90comment, f90nocomment},
		{"fortran", ".f77", f77comment, f77nocomment},
		{"fortran", ".f", f77comment, f77nocomment},
	}

	var perr error
	podheader, perr = regexp.Compile("^=[a-zA-Z]")
	if perr != nil {
		panic(perr)
	}

	neverInterestingByPrefix = []string{"."}
	neverInterestingByInfix = []string{".so.", "/."}
	ignoreSuffixes := []string{"~",
		".a", ".la", ".o", ".so", ".ko",
		".gif", ".jpg", ".jpeg", ".ico", ".xpm", ".xbm", ".bmp",
		".ps", ".pdf", ".eps",
		".tfm", ".ttf", ".bdf", ".afm",
		".fig", ".pic",
		".pyc", ".pyo", ".elc",
		".1", ".2", ".3", ".4", ".5", ".6", ".7", ".8", ".n", ".man",
		".html", ".htm", ".sgml", ".xml",
		".txt", ".tex", ".texi",
		".po",
		".gz", ".bz2", ".Z", ".tgz", ".zip",
		".au", ".wav", ".ogg",
	}
	neverInterestingBySuffix = make(map[string]bool)
	for i := range ignoreSuffixes {
		neverInterestingBySuffix[ignoreSuffixes[i]] = true
	}
	neverInterestingByBasename = map[string]bool{
		"readme": true, "readme.tk": true, "readme.md": true,
		"changelog": true, "repository": true, "changes": true,
		"bugs": true, "todo": true, "copying": true, "maintainers": true,
		"news":      true,
		"configure": true, "autom4te.cache": true, "config.log": true,
		"config.status": true,
		"lex.yy.c":      true, "lex.yy.cc": true,
		"y.code.c": true, "y.tab.c": true, "y.tab.h": true,
	}
	cHeaderPriority = []string{"c", "c++", "obj-c"}

	generated = "automatically generated|generated automatically|generated by|a lexical scanner generated by flex|this is a generated file|generated with the.*utility|do not edit|do not hand-hack"

}

// Generic machinery for walking source text to count lines

/* Modes */
const NORMAL = 0
const INSTRING = 1
const INMULTISTRING = 2
const INCOMMENT = 3

type countContext struct {
	line                  []byte
	line_number           uint
	nonblank              bool // Is current line nonblank?
	lexfile               bool // Do we see lex directives?
	last_char_was_newline bool
	underlyingStream      *os.File
	rc                    *bufio.Reader
}

func (ctx *countContext) setup(path string) bool {
	var err error
	ctx.underlyingStream, err = os.Open(path)
	if err != nil {
		log.Println(err)
		return false
	}
	ctx.rc = bufio.NewReader(ctx.underlyingStream)
	ctx.line_number = 1
	return true
}

func (ctx *countContext) teardown() {
	ctx.underlyingStream.Close()
}

// consume - conditionally consume an expected byte sequence
func (ctx *countContext) consume(expect []byte) bool {
	s, err := ctx.rc.Peek(len(expect))
	if err == nil && bytes.Equal(s, expect) {
		ctx.rc.Discard(len(expect))
		return true
	}
	return false
}

func (ctx *countContext) ispeek(c byte) bool {
	if s, err := ctx.rc.Peek(1); err == nil && s[0] == c {
		return true
	}
	return false
}

// getachar - Get one character, tracking line number
func (ctx *countContext) getachar() (byte, error) {
	c, err := ctx.rc.ReadByte()
	if err != nil && err != io.EOF {
		panic("error while reading a character")
	}
	if ctx.last_char_was_newline {
		ctx.line_number++
	}
	if c == '\n' {
		ctx.last_char_was_newline = true
	} else {
		ctx.last_char_was_newline = false
	}
	return c, err
}

// Consume the remainder of a line, updating the line counter
func (ctx *countContext) munchline() bool {
	line, err := ctx.rc.ReadBytes('\n')
	if err == nil {
		ctx.line_number++
		ctx.line = line
		return true
	} else if err == io.EOF {
		return false
	} else {
		panic(err)
	}
}

// Consume the remainder of a line, updating the line counter
func (ctx *countContext) drop(excise string) bool {
	cre, err := regexp.Compile(excise)
	if err != nil {
		panic(fmt.Sprintf("unexpected failure %s while compiling %s", err, excise))
	}
	return cre.ReplaceAllLiteral(ctx.line, []byte("")) != nil
}

// matchline - does a given regexp match the last line read?
func (ctx *countContext) matchline(re string) bool {
	cre, err := regexp.Compile(re)
	if err != nil {
		panic(fmt.Sprintf("unexpected failure %s while compiling %s", err, re))
	}
	return cre.Find(ctx.line) != nil
}

func isspace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f'
}

// Verifier functions for checking that files with disputed extensions
// are actually of the types we think they are.

// really_is_objc - returns TRUE if filename contents really are objective-C.
func really_is_objc(ctx *countContext, path string) bool {
	var is_objc bool = false // Value to determine.
	var brace_lines int      // Lines that begin/end with curly braces.
	var plus_minus int       // Lines that begin with + or -.
	var word_main int        // Did we find "main("?
	var special bool = false // Did we find a special Objective-C pattern?

	ctx.setup(path)
	defer ctx.teardown()

	for ctx.munchline() {
		if ctx.matchline("^\\s*[{}]") || ctx.matchline("[{}];?\\s*") {
			brace_lines++
		}
		if ctx.matchline("^\\s*[+-]") {
			plus_minus++
		}
		if ctx.matchline("\\bmain\\s*\\(") { // "main" followed by "("?
			word_main++
		}
		// Handle /usr/src/redhat/BUILD/egcs-1.1.2/gcc/objc/linking.m:
		if ctx.matchline("(?i)^\\s*\\[object name\\];\\s*") {
			special = true
		}

		if (brace_lines > 1) && ((plus_minus > 1) || word_main > 0 || special) {
			is_objc = true
		}

	}

	if debug > 0 {
		log.Printf("objc verifier returned %t on %s\n", is_objc, path)
	}

	return is_objc
}

func has_keywords(ctx *countContext, path string, lang string, tells []string) bool {
	var matching bool = false // Value to determine.

	ctx.setup(path)
	defer ctx.teardown()

	for ctx.munchline() {
		for i := range tells {
			if ctx.matchline(tells[i]) {
				matching = true
				break
			}
		}
	}

	if debug > 0 {
		log.Printf("%s verifier returned %t on %s\n",
			lang, matching, path)
	}

	return matching
}

// really_is_occam - returns TRUE if filename contents really are occam.
func really_is_occam(ctx *countContext, path string) bool {
	return has_keywords(ctx, path, "occam", []string{"--", "PROC"})
}

// really_is_lex - returns TRUE if filename contents really are lex.
func really_is_lex(ctx *countContext, path string) bool {
	return has_keywords(ctx, path, "lex", []string{"%{", "%%", "%}"})
}

// really_is_pop11 - returns TRUE if filename contents really are pop11.
func really_is_pop11(ctx *countContext, path string) bool {
	return has_keywords(ctx, path, "pop11", []string{"define", "printf"})
}

// really_is_sather - returns TRUE if filename contents really are sather.
func really_is_sather(ctx *countContext, path string) bool {
	return has_keywords(ctx, path, "sather", []string{"class"})
}

// really_is_prolog - returns TRUE if filename contents really are prolog.
// Without this check, Perl files will be falsely identified.
func really_is_prolog(ctx *countContext, path string) bool {
	ctx.setup(path)
	defer ctx.teardown()

	for ctx.munchline() {
		if bytes.HasPrefix(ctx.line, []byte("#")) {
			return false
		} else if ctx.matchline("\\$[[:alpha]]") {
			return false
		}
	}

	return true
}

// really_is_expect - filename, returns true if its contents really are Expect.
//
// dwheeler had this to say:
//
// Many "exp" files (such as in Apache and Mesa) are just "export" data,
// summarizing something else (e.g., its interface).
// Sometimes (like in RPM) it's just misc. data.
// Thus, we need to look at the file to determine
// if it's really an "expect" file.
// The heuristic is as follows: it's Expect _IF_ it:
// 1. has "load_lib" command and either "#" comments or {}.
// 2. {, }, and one of: proc, if, [...], expect
func really_is_expect(ctx *countContext, path string) bool {
	var is_expect = false // Value to determine.

	var begin_brace bool // Lines that begin with curly braces.
	var end_brace bool   // Lines that begin with curly braces.
	var load_lib bool    // Lines with the Load_lib command.
	var found_proc bool
	var found_if bool
	var found_brackets bool
	var found_expect bool
	var found_pound bool

	ctx.setup(path)
	defer ctx.teardown()

	for ctx.munchline() {
		if ctx.matchline("#") {
			found_pound = true
			// Delete trailing comments
			i := bytes.Index(ctx.line, []byte("#"))
			if i > -1 {
				ctx.line = ctx.line[:i]
			}
		}

		if ctx.matchline("^\\s*\\{") {
			begin_brace = true
		}
		if ctx.matchline("\\{\\s*$") {
			begin_brace = true
		}
		if ctx.matchline("^\\s*}") {
			end_brace = true
		}
		if ctx.matchline("};?\\s*$") {
			end_brace = true
		}
		if ctx.matchline("^\\s*load_lib\\s+\\S") {
			load_lib = true
		}
		if ctx.matchline("^\\s*proc\\s") {
			found_proc = true
		}
		if ctx.matchline("^\\s*if\\s") {
			found_if = true
		}
		if ctx.matchline("\\[.*\\]") {
			found_brackets = true
		}
		if ctx.matchline("^\\s*expect\\s") {
			found_expect = true
		}
	}

	if load_lib && (found_pound || (begin_brace && end_brace)) {
		is_expect = true
	}
	if begin_brace && end_brace &&
		(found_proc || found_if || found_brackets || found_expect) {
		is_expect = true
	}

	if debug > 0 {
		log.Printf("expect verifier returned %t on %s\n", is_expect, path)
	}

	return is_expect
}

// really-is-pascal - returns  true if filename contents really are Pascal.
func really_is_pascal(ctx *countContext, path string) bool {
	//
	// dwheeler had this to say:
	//
	// This isn't as obvious as it seems.
	// Many ".p" files are Perl files
	// (such as /usr/src/redhat/BUILD/ispell-3.1/dicts/czech/glob.p),
	// others are C extractions
	// (such as /usr/src/redhat/BUILD/linux/include/linux/umsdos_fs.p
	// and some files in linuxconf).
	// However, test files in "p2c" really are Pascal, for example.
	//
	// Note that /usr/src/redhat/BUILD/ucd-snmp-4.1.1/ov/bitmaps/UCD.20.p
	// is actually C code.  The heuristics determine that they're not Pascal,
	// but because it ends in ".p" it's not counted as C code either.
	// I believe this is actually correct behavior, because frankly it
	// looks like it's automatically generated (it's a bitmap expressed as code).
	// Rather than guess otherwise, we don't include it in a list of
	// source files.  Let's face it, someone who creates C files ending in ".p"
	// and expects them to be counted by default as C files in SLOCCount needs
	// their head examined.  I suggest examining their head
	// with a sucker rod (see syslogd(8) for more on sucker rods).
	//
	// This heuristic counts as Pascal such files such as:
	//  /usr/src/redhat/BUILD/teTeX-1.0/texk/web2c/tangleboot.p
	// Which is hand-generated.  We don't count woven documents now anyway,
	// so this is justifiable.

	// The heuristic is as follows: it's Pascal _IF_ it has all of the following
	// (ignoring {...} and (*...*) comments):
	// 1. "^..program NAME" or "^..unit NAME",
	// 2. "procedure", "function", "^..interface", or "^..implementation",
	// 3. a "begin", and
	// 4. it ends with "end.",
	//
	// Or it has all of the following:
	// 1. "^..module NAME" and
	// 2. it ends with "end.".
	//
	// Or it has all of the following:
	// 1. "^..program NAME",
	// 2. a "begin", and
	// 3. it ends with "end.".
	//
	// The "end." requirements in particular filter out non-Pascal.
	//
	// Note (jgb): this does not detect Pascal main files in fpc, like
	// fpc-1.0.4/api/test/testterminfo.pas, which does not have "program" in
	// it
	var is_pascal bool // Value to determine.

	var has_program bool
	var has_unit bool
	var has_module bool
	var has_procedure_or_function bool
	var has_begin bool
	var found_terminating_end bool

	ctx.setup(path)
	defer ctx.teardown()

	for ctx.munchline() {
		// Ignore {...} comments on this line; imperfect, but effective.
		ctx.drop("\\{.*?\\}")
		// Ignore (*...*) comments on this line; imperfect but effective.
		ctx.drop("\\(\\*.*\\*\\)")

		if ctx.matchline("(?i)\\bprogram\\s+[A-Za-z]") {
			has_program = true
		}
		if ctx.matchline("(?i)\\bunit\\s+[A-Za-z]") {
			has_unit = true
		}
		if ctx.matchline("(?i)\\bmodule\\s+[A-Za-z]") {
			has_module = true
		}
		if ctx.matchline("(?i)\\bprocedure\\b") {
			has_procedure_or_function = true
		}
		if ctx.matchline("(?i)\\bfunction\\b") {
			has_procedure_or_function = true
		}
		if ctx.matchline("(?i)^\\s*interface\\s+") {
			has_procedure_or_function = true
		}
		if ctx.matchline("(?i)^\\s*implementation\\s+") {
			has_procedure_or_function = true
		}
		if ctx.matchline("(?i)\\bbegin\\b") {
			has_begin = true
		}
		// Originally dw said: "This heuristic fails if there
		// are multi-line comments after "end."; I haven't
		// seen that in real Pascal programs:"
		// But jgb found there are a good quantity of them in
		// Debian, specially in fpc (at the end of a lot of
		// files there is a multiline comment with the
		// changelog for the file).  Therefore, assume Pascal
		// if "end." appears anywhere in the file.
		if ctx.matchline("(?i)end\\.\\s*$") {
			found_terminating_end = true
		}
	}

	// Okay, we've examined the entire file looking for clues;
	// let's use those clues to determine if it's really Pascal:
	is_pascal = (((has_unit || has_program) && has_procedure_or_function &&
		has_begin && found_terminating_end) ||
		(has_module && found_terminating_end) ||
		(has_program && has_begin && found_terminating_end))

	if debug > 0 {
		log.Printf("pascal verifier returned %t on %s\n", is_pascal, path)
	}

	return is_pascal
}

func was_generated_automatically(ctx *countContext, path string, eolcomment string) bool {
	// Determine if the file was generated automatically.
	// Use a simple heuristic: check if first few lines have phrases like
	// "generated automatically", "automatically generated", "Generated by",
	// or "do not edit" as the first
	// words in the line (after possible comment markers and spaces).
	i := 15 // Look at first 15 lines.
	ctx.setup(path)
	defer ctx.teardown()

	// Avoid blowing up if the comment leader is "*" (as in COBOL).
	if eolcomment == "*" {
		eolcomment = ""
	} else {
		eolcomment = "|" + eolcomment
	}
	re := "(\\*" + eolcomment + ").*(?i:" + generated + ")"
	cre, err := regexp.Compile(re)
	if err != nil {
		panic(fmt.Sprintf("unexpected failure while building %s", re))
	}

	for ctx.munchline() && i > 0 {
		//log.Printf("Matching %s against %s", ctx.line, re)
		if cre.Find(ctx.line) != nil {
			if debug > 0 {
				log.Printf("%s: is generated\n", path)
			}
			return true
		}
		i--
	}

	return false
}

// hashbang - hunt for a specified string in the first line of an executable
func hashbang(ctx *countContext, path string, langname string) bool {
	fi, err := os.Stat(path)
	// If it's not executable by somebody, don't read for hashbang
	if err != nil || (fi.Mode()&01111) == 0 {
		return false
	}
	ctx.setup(path)
	defer ctx.teardown()
	s, err := ctx.rc.ReadString('\n')
	return err == nil && strings.HasPrefix(s, "#!") && strings.Contains(s, langname)
}

// c_family_counter - Count the SLOC in a C-family source file
//
// C++ headers get counted as C. This can only be fixed in postprocessing
// by noticing that there are no files with a C extension in the tree.
//
// Another minor issue is that it's possible for the antecedents in Lex rules
// to look like C comment starts. In theory we could fix this by requiring Lex
// files to contain %%.
func c_family_counter(ctx *countContext, path string, syntax genericLanguage) uint {
	/* Types of comments: */
	const BLOCK_COMMENT = 0
	const TRAILING_COMMENT = 1

	var sloc uint = 0
	var mode int = NORMAL /* NORMAL, INSTRING, INMULTISTRING, or INCOMMENT */
	var comment_type int  /* BLOCK_COMMENT or TRAILING_COMMENT */
	var startline uint

	if syntax.verifier != nil && !syntax.verifier(ctx, path) {
		return 0
	}

	ctx.setup(path)
	defer ctx.teardown()

	for {
		c, err := ctx.getachar()
		if err == io.EOF {
			break
		}

		if mode == NORMAL {
			if !ctx.lexfile && c == '"' {
				ctx.nonblank = true
				mode = INSTRING
				startline = ctx.line_number
			} else if !ctx.lexfile && c == '\'' {
				/* Consume single-character 'xxxx' values */
				ctx.nonblank = true
				c, err = ctx.getachar()
				if c == '\\' {
					c, err = ctx.getachar()
				}
				for {
					c, err = ctx.getachar()
					if (c == '\'') || (c == '\n') || (err == io.EOF) {
						break
					}
				}
			} else if (c == syntax.commentleader[0]) && (ctx.ispeek(syntax.commentleader[1])) {
				c, err = ctx.getachar()
				mode = INCOMMENT
				comment_type = BLOCK_COMMENT
				startline = ctx.line_number
			} else if (syntax.eolcomment != "") && c == syntax.eolcomment[0] && (len(syntax.eolcomment) > 1 && ctx.ispeek(syntax.eolcomment[1])) {
				c, err = ctx.getachar()
				mode = INCOMMENT
				comment_type = TRAILING_COMMENT
				startline = ctx.line_number
			} else if (syntax.multistring != "") && (c == syntax.multistring[0]) {
				mode = INMULTISTRING
				startline = ctx.line_number
			} else if !isspace(c) {
				ctx.nonblank = true
			}
		} else if mode == INSTRING {
			// We only count string lines with non-whitespace --
			// this is to gracefully handle syntactically invalid
			// programs.  You could argue that multiline strings
			// with whitespace are still executable and should be
			// counted.
			if !isspace(c) {
				ctx.nonblank = true
			}
			if c == '"' {
				mode = NORMAL
			} else if (c == '\\') && (ctx.ispeek('"') || ctx.ispeek('\\')) {
				c, err = ctx.getachar()
			} else if (c == '\\') && ctx.ispeek('\n') {
				c, err = ctx.getachar()
			} else if c == '\n' {
				// We found a bare newline in a string without
				// preceding backslash.
				if syntax.eolwarn {
					log.Printf("WARNING - newline in string, line %d, file %s\n", ctx.line_number, path)
				}

				// We COULD warn & reset mode to
				// "Normal", but lots of code does this,
				// so we'll just depend on the warning
				// for ending the program in a string to
				// catch syntactically erroneous
				// programs.
			}
		} else if mode == INMULTISTRING {
			// We only count multi-string lines with non-whitespace.
			if !isspace(c) {
				ctx.nonblank = true
			}
			if c == syntax.multistring[0] {
				mode = NORMAL
			}
		} else { /* INCOMMENT mode */
			if (c == '\n') && (comment_type == TRAILING_COMMENT) {
				mode = NORMAL
			}
			if (comment_type == BLOCK_COMMENT) && (c == syntax.commenttrailer[0]) && ctx.ispeek(syntax.commenttrailer[1]) {
				c, err = ctx.getachar()
				mode = NORMAL
			}
		}
		if c == '\n' {
			if ctx.nonblank {
				sloc++
			}
			ctx.nonblank = false
			if ctx.consume([]byte("%")) {
				ctx.lexfile = true
				ctx.nonblank = true
			}
		}
	}
	/* We're done with the file.  Handle EOF-without-EOL. */
	if ctx.nonblank {
		sloc++
	}
	ctx.nonblank = false
	if (mode == INCOMMENT) && (comment_type == TRAILING_COMMENT) {
		mode = NORMAL
	}

	if mode == INCOMMENT {
		log.Printf("\"%s\", line %d: ERROR - terminated in comment beginning here\n",
			path, startline)
	} else if mode == INSTRING {
		log.Printf("\"%s\", line %d: ERROR - terminated in string beginning here\n",
			path, startline)
	}

	return sloc
}

// genericCounter - count SLOC in a generic language.
func genericCounter(ctx *countContext,
	path string, eolcomment string,
	verifier func(*countContext, string) bool) uint {
	var sloc uint = 0

	if verifier != nil && !verifier(ctx, path) {
		return 0
	}

	ctx.setup(path)
	defer ctx.teardown()

	for ctx.munchline() {
		i := bytes.Index(ctx.line, []byte(eolcomment))
		if i > -1 {
			ctx.line = ctx.line[:i]
		}
		ctx.line = bytes.Trim(ctx.line, " \t\r\n")
		if len(ctx.line) > 0 {
			sloc++
		}
	}

	return sloc
}

func pythonCounter(ctx *countContext, path string) uint {
	var sloc uint = 0
	var isintriple bool  // A triple-quote is in effect.
	var isincomment bool // We are in a multiline (triple-quoted) comment.

	ctx.setup(path)
	defer ctx.teardown()

	triple_boundary := func(line []byte) bool { return bytes.Contains(line, []byte(dt)) || bytes.Contains(line, []byte(st)) }
	for ctx.munchline() {
		// Delete trailing comments
		i := bytes.Index(ctx.line, []byte("#"))
		if i > -1 {
			ctx.line = ctx.line[:i]
		}

		if !isintriple { // Normal case:
			// Ignore triple-quotes that begin & end on the ctx.line.
			ctx.line = dtriple.ReplaceAllLiteral(ctx.line, []byte(""))
			ctx.line = striple.ReplaceAllLiteral(ctx.line, []byte(""))
			// Delete lonely strings starting on BOL.
			ctx.line = dlonely.ReplaceAllLiteral(ctx.line, []byte(""))
			ctx.line = slonely.ReplaceAllLiteral(ctx.line, []byte(""))
			// Delete trailing comments
			i := bytes.Index(ctx.line, []byte("#"))
			if i > -1 {
				ctx.line = ctx.line[:i]
			}
			// Does multi-line triple-quote begin here?
			if triple_boundary(ctx.line) {
				isintriple = true
				ctx.line = bytes.Trim(ctx.line, " \t\r\n")
				// It's a comment if at BOL.
				if bytes.HasPrefix(ctx.line, []byte(dt)) || bytes.HasPrefix(ctx.line, []byte(st)) {
					isincomment = true
				}
			}
		} else { // we ARE in a triple.
			if triple_boundary(ctx.line) {
				if isincomment {
					// Delete text if it's a comment (not if data)
					ctx.line = dtrailer.ReplaceAllLiteral(ctx.line, []byte(""))
					ctx.line = strailer.ReplaceAllLiteral(ctx.line, []byte(""))
				} else {
					// Leave something there to count.
					ctx.line = dtrailer.ReplaceAllLiteral(ctx.line, []byte("x"))
					ctx.line = strailer.ReplaceAllLiteral(ctx.line, []byte("x"))
				}
				// But wait!  Another triple might
				// start on this ctx.line!  (see
				// Python-1.5.2/Tools/freeze/makefreeze.py
				// for an example)
				if triple_boundary(ctx.line) {
					// It did!  No change in state!
				} else {
					isintriple = false
					isincomment = false
				}
			}
		}
		ctx.line = bytes.Trim(ctx.line, " \t\r\n")
		if !isincomment && len(ctx.line) > 0 {
			sloc++
		}
	}

	return sloc
}

// perlCounter - count SLOC in Perl
//
// Physical lines of Perl are MUCH HARDER to count than you'd think.
// Comments begin with "#".
// Also, anything in a "perlpod" is a comment.
// See perlpod(1) for more info; a perlpod starts with
// \s*=command, can have more commands, and ends with \s*=cut.
// Note that = followed by space is NOT a perlpod.
// Although we ignore everything after __END__ in a file,
// we will count everything after __DATA__; there's arguments for counting
// and for not counting __DATA__.
//
// What's worse, "here" documents must be COUNTED AS CODE, even if
// they're FORMATTED AS A PERLPOD.  Surely no one would do this, right?
// Sigh... it can happen. See perl5.005_03/pod/splitpod.
func perlCounter(ctx *countContext, path string) uint {
	var sloc uint = 0
	var heredoc string
	var isinpod bool

	ctx.setup(path)
	defer ctx.teardown()

	for ctx.munchline() {
		// Delete trailing comments
		i := bytes.Index(ctx.line, []byte("#"))
		if i > -1 {
			ctx.line = ctx.line[:i]
		}

		ctx.line = bytes.Trim(ctx.line, " \t\r\n")

		if heredoc != "" && strings.HasPrefix(string(ctx.line), heredoc) {
			heredoc = "" //finished here doc.
		} else if i := bytes.Index(ctx.line, []byte("<<")); i > -1 {
			// Beginning of a here document.
			heredoc = string(bytes.Trim(ctx.line[i:], "< \t\"';,"))
		} else if len(heredoc) == 0 && bytes.HasPrefix(ctx.line, []byte("=cut")) {
			// Ending a POD?
			if !isinpod {
				log.Printf("\"%s\", %d: cut without pod start\n",
					path, ctx.line_number)
			}
			isinpod = false
			continue // Don't count the cut command.
		} else if len(heredoc) == 0 && podheader.Match(ctx.line) {
			// Starting or continuing a POD?
			// Perlpods can have multiple contents, so
			// it's okay if isinpod == true.  Note that
			// =(space) isn't a POD; library file
			// perl5db.pl does this!
			isinpod = true
		} else if bytes.HasPrefix(ctx.line, []byte("__END__")) {
			// Stop processing this file on __END__.
			break
		}
		if !isinpod && len(ctx.line) > 0 {
			sloc++
		}
	}

	return sloc
}

// pascalCounter - Handle lanuages like Pascal and Modula 3
func pascalCounter(ctx *countContext, path string, syntax pascalLike) uint {
	var sloc uint = 0
	var mode int = NORMAL /* NORMAL, or INCOMMENT */
	var startline uint

	if syntax.verifier != nil && !syntax.verifier(ctx, path) {
		return 0
	}

	ctx.setup(path)
	defer ctx.teardown()

	for {
		c, err := ctx.getachar()
		if err == io.EOF {
			break
		}

		if mode == NORMAL {
			if syntax.bracketcomments && c == '{' {
				mode = INCOMMENT
			} else if (c == '(') && ctx.ispeek('*') {
				c, err = ctx.getachar()
				mode = INCOMMENT
			} else if !isspace(c) {
				ctx.nonblank = true
			} else if c == '\n' {
				if ctx.nonblank {
					sloc++
				}
				ctx.nonblank = false
			}
		} else { /* INCOMMENT mode */
			if syntax.bracketcomments && c == '}' {
				mode = NORMAL
			} else if (c == '*') && ctx.ispeek(')') {
				c, err = ctx.getachar()
				mode = NORMAL
			}
		}
	}
	/* We're done with the file.  Handle EOF-without-EOL. */
	if ctx.nonblank {
		sloc++
	}
	ctx.nonblank = false

	if mode == INCOMMENT {
		log.Printf("\"%s\", line %d: ERROR - terminated in comment beginning here.\n",
			path, startline)
	} else if mode == INSTRING {
		log.Printf("\"%s\", line %d: ERROR - terminated in string beginning here.\n",
			path, startline)
	}

	return sloc
}

func fortranCounter(ctx *countContext, path string, syntax fortranLike) uint {
	var sloc uint

	ctx.setup(path)
	defer ctx.teardown()

	for ctx.munchline() {
		if !(syntax.comment.Match(ctx.line) && !syntax.nocomment.Match(ctx.line)) {
			sloc++
		}
	}
	return sloc
}

// Generic - recognize lots of languages with generic syntax
func Generic(ctx *countContext, path string) SourceStat {
	var stat SourceStat

	autofilter := func(eolcomment string) bool {
		if was_generated_automatically(ctx, path, eolcomment) {
			if debug > 0 {
				fmt.Printf("automatic generation filter failed: %s\n", path)
			}
			return true
		}
		if debug > 0 {
			fmt.Printf("automatic generation filter passed: %s\n", path)
		}
		return false
	}

	for i := range genericLanguages {
		lang := genericLanguages[i]
		if strings.HasSuffix(path, lang.suffix) {
			if autofilter(lang.eolcomment) {
				return stat
			} else if len(lang.commentleader) > 0 {
				stat.SLOC = c_family_counter(ctx, path, lang)
			} else {
				stat.SLOC = genericCounter(ctx, path,
					lang.eolcomment, lang.verifier)
			}
			if stat.SLOC > 0 {
				stat.Language = lang.name
				return stat
			}
		}
	}

	if strings.HasSuffix(path, ".py") || hashbang(ctx, path, "python") {
		if autofilter("#") {
			return stat
		}
		stat.Language = "python"
		stat.SLOC = pythonCounter(ctx, path)
		return stat
	}

	if strings.HasSuffix(path, ".pl") || strings.HasSuffix(path, ".pm") || strings.HasSuffix(path, ".ph") || hashbang(ctx, path, "perl") {
		if autofilter("#") {
			return stat
		}
		stat.Language = "perl"
		stat.SLOC = perlCounter(ctx, path)
		return stat
	}

	if filepath.Base(path) == "wscript" {
		if autofilter("#") {
			return stat
		}
		stat.Language = "waf"
		stat.SLOC = pythonCounter(ctx, path)
		return stat
	}

	for i := range scriptingLanguages {
		if autofilter("#") {
			return stat
		}
		lang := scriptingLanguages[i]
		if strings.HasSuffix(path, lang.suffix) || hashbang(ctx, path, lang.hashbang) {
			stat.Language = lang.name
			stat.SLOC = genericCounter(ctx, path, "#", nil)
			return stat
		}
	}

	for i := range pascalLikes {
		lang := pascalLikes[i]
		if strings.HasSuffix(path, lang.suffix) {
			stat.Language = lang.name
			stat.SLOC = pascalCounter(ctx, path, lang)
			if stat.SLOC > 0 {
				return stat
			}
		}
	}

	for i := range fortranLikes {
		lang := fortranLikes[i]
		if strings.HasSuffix(path, lang.suffix) {
			stat.Language = lang.name
			stat.SLOC = fortranCounter(ctx, path, lang)
			if stat.SLOC > 0 {
				return stat
			}
		}
	}

	return stat
}

func isDirectory(path string) bool {
	fileInfo, err := os.Stat(path)
	return err == nil && fileInfo.Mode().IsDir()
}

func isRegular(path string) bool {
	fileInfo, err := os.Stat(path)
	return err == nil && fileInfo.Mode().IsRegular()
}

// filter - winnows out uninteresting paths before handing them to process
func filter(path string, info os.FileInfo, err error) error {
	if debug > 0 {
		fmt.Printf("entering filter: %s\n", path)
	}
	suffix := filepath.Ext(path)
	if suffix != "" && neverInterestingBySuffix[suffix] {
		if debug > 0 {
			fmt.Printf("suffix filter failed: %s\n", path)
		}
		return err
	}
	for i := range neverInterestingByPrefix {
		if strings.HasPrefix(path, neverInterestingByPrefix[i]) {
			if debug > 0 {
				fmt.Printf("prefix filter failed: %s\n", path)
			}
			return err
		}
	}
	for i := range neverInterestingByInfix {
		if strings.Contains(path, neverInterestingByInfix[i]) {
			if debug > 0 {
				fmt.Printf("infix filter failed: %s\n", path)
			}
			if isDirectory(path) {
				if debug > 0 {
					fmt.Printf("directory skipped: %s\n", path)
				}
				return filepath.SkipDir
			} else {
				return err
			}
		}
	}
	basename := filepath.Base(path)
	if neverInterestingByBasename[strings.ToLower(basename)] {
		if debug > 0 {
			fmt.Printf("basename filter failed: %s\n", path)
		}
		return err
	}
	for i := range exclusions {
		if path == exclusions[i] || strings.HasPrefix(path, exclusions[i]+"/") {
			if debug > 0 {
				fmt.Printf("exclusion '%s' filter failed: %s\n", exclusions, path)
			}
			return err
		}
	}

	/* has to come after the infix check for directory */
	if !isRegular(path) {
		if debug > 0 {
			fmt.Printf("regular-file filter failed: %s\n", path)
		}
		return err
	}

	/* toss generated Makefiles */
	if basename == "Makefile" {
		if _, err := os.Stat(path + ".in"); err == nil {
			if debug > 0 {
				fmt.Printf("generated-makefile filter failed: %s\n", path)
			}
			return err
		}
	}

	if debug > 0 {
		fmt.Printf("passed filter: %s\n", path)
	}

	// Now the real work gets done
	ctx := new(countContext)
	st := Generic(ctx, path)
	st.Path = path
	pipeline <- st

	return err
}

type countRecord struct {
	language  string
	linecount uint
	filecount uint
}

func reportCocomo(sloc uint) {
	const TIME_MULT = 2.4
	const TIME_EXP = 1.05
	const SCHED_MULT = 2.5
	const SCHED_EXP = 0.38
	const SALARY = 60384 // From payscale.com, late 2016
	const OVERHEAD = 2.40
	fmt.Printf("Total Physical Source Lines of Code (SLOC)                = %d\n", sloc)
	person_months := TIME_MULT * math.Pow(float64(sloc)/1000, TIME_EXP)
	fmt.Printf("Development Effort Estimate, Person-Years (Person-Months) = %2.2f (%2.2f)\n", person_months/12, person_months)
	fmt.Printf(" (Basic COCOMO model, Person-Months = %2.2f * (KSLOC**%2.2f))\n", TIME_MULT, TIME_EXP)
	sched_months := SCHED_MULT * math.Pow(person_months, SCHED_EXP)
	fmt.Printf("Schedule Estimate, Years (Months)                         = %2.2f (%2.2f)\n", sched_months/12, sched_months)
	fmt.Printf(" (Basic COCOMO model, Months = %2.2f * (person-months**%2.2f))\n", SCHED_MULT, SCHED_EXP)
	fmt.Printf("Estimated Average Number of Developers (Effort/Schedule)  = %2.2f\n", person_months/sched_months)
	fmt.Printf("Total Estimated Cost to Develop                           = $%d\n", int(SALARY*(person_months/12)*OVERHEAD))
	fmt.Printf(" (average salary = $%d/year, overhead = %2.2f).\n", SALARY, OVERHEAD)
}

func listLanguages() []string {
	var names []string = []string{"python", "waf", "perl"}
	var lastlang string
	for i := range genericLanguages {
		lang := genericLanguages[i].name
		if lang != lastlang {
			names = append(names, lang)
			lastlang = lang
		}
	}

	for i := range scriptingLanguages {
		lang := scriptingLanguages[i].name
		if lang != lastlang {
			names = append(names, lang)
			lastlang = lang
		}
	}

	for i := range pascalLikes {
		lang := pascalLikes[i].name
		if lang != lastlang {
			names = append(names, lang)
			lastlang = lang
		}
	}

	for i := range fortranLikes {
		lang := fortranLikes[i].name
		if lang != lastlang {
			names = append(names, lang)
			lastlang = lang
		}
	}
	sort.Strings(names)
	return names
}

func listExtensions() {
	extensions := map[string][]string{
		"python": {".py"},
		"waf":    {"waf"},
		"perl":   {"pl", "pm"},
	}
	for i := range genericLanguages {
		lang := genericLanguages[i]
		extensions[lang.name] = append(extensions[lang.name], lang.suffix)
	}

	for i := range scriptingLanguages {
		lang := scriptingLanguages[i]
		extensions[lang.name] = append(extensions[lang.name], lang.suffix)
	}

	for i := range pascalLikes {
		lang := pascalLikes[i]
		extensions[lang.name] = append(extensions[lang.name], lang.suffix)
	}

	for i := range fortranLikes {
		lang := fortranLikes[i]
		extensions[lang.name] = append(extensions[lang.name], lang.suffix)
	}
	names := listLanguages()
	for i := range names {
		fmt.Printf("%s: %v\n", names[i], extensions[names[i]])
	}
}

type sortable []countRecord

func (a sortable) Len() int           { return len(a) }
func (a sortable) Swap(i int, j int)  { a[i], a[j] = a[j], a[i] }
func (a sortable) Less(i, j int) bool { return -a[i].linecount < -a[j].linecount }

var cpuprofile = flag.String("cpuprofile", "", "write cpu profile to file")

func main() {
	var individual bool
	var unclassified bool
	var list bool
	var extensions bool
	var cocomo bool
	var json bool
	var showversion bool
	excludePtr := flag.String("x", "",
		"paths and directories to exclude")
	flag.BoolVar(&individual, "i", false,
		"list counts and types for individual files")
	flag.BoolVar(&unclassified, "u", false,
		"list unclassified files")
	flag.BoolVar(&cocomo, "c", false,
		"report Cocomo-model estimation")
	flag.BoolVar(&list, "l", false,
		"list supported languages and exit")
	flag.BoolVar(&extensions, "e", false,
		"list extensions associated with each language and exit")
	flag.IntVar(&debug, "d", 0,
		"set debug level")
	flag.BoolVar(&json, "j", false,
		"dump statistics in JSON format")
	flag.BoolVar(&showversion, "V", false,
		"report version and exil")
	flag.Parse()

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	if showversion {
		fmt.Printf("loccount %.1f\n", version)
		return
	} else if list {
		fmt.Printf("%s\n", listLanguages())
		return
	} else if extensions {
		listExtensions()
		return
	}

	individual = individual || unclassified

	// For maximum performance, make the pipeline be as deep as the
	// number of processor we have available, that way the machine will
	// be running full-out exactly when it's filled and no sooner.
	// This makes order of output nondeterministic, which is why
	// we sometimes want to disable it.
	var chandepth int
	if individual || unclassified {
		chandepth = 0
	} else {
		chandepth = runtime.NumCPU()
	}
	pipeline = make(chan SourceStat, chandepth)

	if len(*excludePtr) > 0 {
		exclusions = strings.Split(*excludePtr, ",")
	}
	roots := flag.Args()

	go func() {
		for i := range roots {
			// The system filepath.Walk() works here,
			// but is slower.
			Walk(roots[i], filter)
		}
		close(pipeline)
	}()

	var totals countRecord
	counts := map[string]countRecord{}

	// Mainline resumes
	for {
		st, more := <-pipeline
		if !more {
			break
		}
		if debug > 0 {
			fmt.Printf("from pipeline: %s %d %s\n",
				st.Path, st.SLOC, st.Language)
		}

		if individual {
			if !unclassified && st.SLOC > 0 {
				fmt.Printf("%s %d %s\n",
					st.Path, st.SLOC, st.Language)
			} else if unclassified && st.SLOC == 0 {
				// Not a recognized source type,
				// nor anything we know to discard
				fmt.Println(st.Path)
			}
			continue
		}

		if st.SLOC > 0 {
			var tmp = counts[st.Language]
			tmp.language = st.Language
			tmp.linecount += st.SLOC
			tmp.filecount++
			counts[st.Language] = tmp
			totals.linecount += st.SLOC
			totals.filecount++
		}
	}

	if individual {
		return
	}

	// C headers may get reassigned based on what other languages
	// are present in the tree
	if counts["c-header"].linecount > 0 {
		for i := range cHeaderPriority {
			if counts[cHeaderPriority[i]].linecount > 0 {
				var tmp = counts[cHeaderPriority[i]]
				tmp.linecount += counts["c-header"].linecount
				counts[cHeaderPriority[i]] = tmp
				delete(counts, "c-header")
				break
			}
		}
	}

	var summary sortable
	totals.language = "all"
	if totals.filecount > 1 {
		summary = append(summary, totals)
	}
	for _, v := range counts {
		summary = append(summary, v)
	}

	sort.Sort(summary)
	for i := range summary {
		r := summary[i]
		if json {
			fmt.Printf("{\"language\":\"%s\", \"linecount\":%d, \"filecount\":%d}\n",
				r.language,
				r.linecount,
				r.filecount)
		} else {
			fmt.Printf("%-12s %7d (%2.2f%%) in %d files\n",
				r.language,
				r.linecount,
				float64(r.linecount)*100.0/float64(totals.linecount),
				r.filecount)
		}
	}

	if cocomo {
		reportCocomo(totals.linecount)
	}
}

// end

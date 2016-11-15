package main

import "bufio"
import "fmt"
import "flag"
import "io"
import "os"
import "path/filepath"
import "strings"
import "log"

// Not yet supported from the sloccount list: asm, Cobol, exp, f90, fortran,
// Haskell, ML, Modula 3, Pascal.
// Known problem: Lisp sources with a .l extension are rare but not unknown.

var exclusions []string
var unclassified bool

type SourceStat struct {
	Language string
	SLOC uint
}

// Generic machinery for walking source text to count lines

/* Modes */
const NORMAL = 0
const INSTRING = 1
const INCOMMENT = 2

/* Globals */
var line_number uint
var last_char_was_newline bool = false
var underlyingStream *os.File
var rc *bufio.Reader
var lastpath string

func peek() byte {
	bytes, err := rc.Peek(1)
	if err != nil {
		panic("error while peeking")
	}
	return bytes[0]
}

func ispeek(c byte) bool {
	if c == peek() {
		return true
	}
	return false
}

// getachar - Get one character, tracking line number
func getachar() (byte, error) {
	c, err := rc.ReadByte()
	if err != nil && err != io.EOF {
		panic("error while reading a character")
	}
	if last_char_was_newline {
		line_number++
	}
	if c == '\n' {
		last_char_was_newline = true
	} else {
		last_char_was_newline = false
	}
	return c, err
}

func isspace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f'
}

func contains(s string, c byte) bool {
	for i := range s {
		if s[i] == c {
			return true
		}
	}
	return false
}

// The purpose of the following two functions is to set up context
// so that we never have to open a source file more than once.

func bufferSetup(path string) bool {
	if len(lastpath) > 0 && (lastpath == path) {
		underlyingStream.Seek(0, 0)
	} else {
		var err error
		underlyingStream, err = os.Open(path)
		lastpath = path
		if err != nil {
			log.Println(err)
			return false
		}
	}
	rc = bufio.NewReader(underlyingStream)
	line_number = 1
	return true
}

func bufferTeardown() {
	underlyingStream.Close()
}

// hashbang - hunt for a specified string in the first line of a file
func hashbang(path string, langname string) bool {
	bufferSetup(path)
	s, err := rc.ReadString('\n')
	return err == nil && strings.HasPrefix(s, "#!") && strings.Contains(s, langname)
}

// C line counting algorithm by David A. Wheeler; Go code by ESR.

/* Types of comments: */
const ANSIC_STYLE = 0
const CPP_STYLE = 1

// sloc_count - Count the SLOC in a C-family source file
func sloc_count(path string) uint {
	var sloc uint = 0
	var sawchar bool = false           /* Did you see a char on this line? */
	var mode int = NORMAL              /* NORMAL, INSTRING, or INCOMMENT */
	var comment_type int = ANSIC_STYLE /* ANSIC_STYLE or CPP_STYLE */

	/*
        The following implements a state machine with transitions; the
        main state is "mode" and "comment_type", the transitions are
	triggered by characters input.
	*/

	for {
		c, err := getachar()
		if err == io.EOF {
			break
		}

		if mode == NORMAL {
			if c == '"' {
				sawchar = true
				mode = INSTRING
			} else if c == '\'' {
				/* Consume single-character 'xxxx' values */
				sawchar = true
				c, err = getachar()
				if c == '\\' {
					c, err = getachar()
				}
				for {
					c, err = getachar()
					if (c == '\'') || (c == '\n') || (err == io.EOF) {
						break
					}
				}
			} else if (c == '/') && ispeek('*') {
				c, err = getachar()
				mode = INCOMMENT
				comment_type = ANSIC_STYLE
			} else if (c == '/') && ispeek('/') {
				c, err = getachar()
				mode = INCOMMENT
				comment_type = CPP_STYLE
			} else if !isspace(c) {
				sawchar = true
			}
		} else if mode == INSTRING {
			/*
		        We only count string lines with non-whitespace --
		        this is to gracefully handle syntactically invalid
			programs.
			You could argue that multiline strings with
			whitespace are still executable and should be
			counted.
			 */
			if !isspace(c) {
				sawchar = true
			}
			if c == '"' {
				mode = NORMAL
			} else if (c == '\\') && (ispeek('"') || ispeek('\\')) {
				c, err = getachar()
			} else if (c == '\\') && ispeek('\n') {
				c, err = getachar()
			} else if (c == '\n') {
				/*
                                We found a bare newline in a string without
				preceding backslash.
                                */
				log.Printf("WARNING - newline in string, line %ld, file %s\n", line_number, path)

				/*
                                We COULD warn & reset mode to
                                "Normal", but lots of code does this,
                                so we'll just depend on the warning
                                for ending the program in a string to
                                catch syntactically erroneous
                                programs.
                                */
			}
		} else { /* INCOMMENT mode */
			if (c == '\n') && (comment_type == CPP_STYLE) {
				mode = NORMAL
			}
			if (comment_type == ANSIC_STYLE) && (c == '*') && ispeek('/') {
				c, err = getachar()
				mode = NORMAL
			}
		}
		if c == '\n' {
			if sawchar {
				sloc++
			}
			sawchar = false
		}
	}
	/* We're done with the file.  Handle EOF-without-EOL. */
	if sawchar {
		sloc++
	}
	sawchar = false
	if (mode == INCOMMENT) && (comment_type == CPP_STYLE) {
		mode = NORMAL
	}

	if mode == INCOMMENT {
		log.Printf("ERROR - terminated in comment in %s\n", path)
	} else if mode == INSTRING {
		log.Printf("ERROR - terminated in string in %s\n", path)
	}

	return sloc
}

// C - recognize files from C-family languages and get linecounts from them.
//
// C++ headers get counted as C. This can only be fixed in postprocessing
// by noticing that there are no files with a C extension in the tree.
// Another minor issue is that it's possible for the antcedents in Lex rules
// to look like C comment starts.
func C(path string) SourceStat {
	var stat SourceStat
	type cLike struct {
		language string
		extension string
	}
	cLikes := []cLike{
		{"C", ".c"},
		{"C", ".h"},
		{"Yacc", ".y"},
		{"Lex", ".l"},
		{"C++", ".cpp"},
		{"C++", ".cxx"},
		{"Objective-C", ".m"},
		{"C#", ".cs"},
		{"Go", ".go"},
	}
	for i := range cLikes {
		lang := cLikes[i]
		if strings.HasSuffix(path, lang.extension) {
			stat.Language = lang.language
			bufferSetup(path)
			defer bufferTeardown()
			stat.SLOC = sloc_count(path)
		}
	}
	return stat
}

// generic_sloc_count - count SLOC in a generic language.
//
// We get to specify a set of possible string delimiters (normally
// a singleton string containing single or double quote, or a doubleton
// containing both). We also get to specify a comment leader.
func generic_sloc_count(path string, stringdelims string, commentleader string) uint {
	var sloc uint = 0
	var sawchar bool = false           /* Did you see a char on this line? */
	var mode int = NORMAL              /* NORMAL, INSTRING, or INCOMMENT */
	var delimseen byte                 /* what string delimiter? */

	bufferSetup(path)
	defer bufferTeardown()
	
	for {
		c, err := getachar()
		if err == io.EOF {
			break
		}

		if mode == NORMAL {
			if contains(stringdelims, c) {
				sawchar = true
				delimseen = c
				mode = INSTRING
			} else if (c == commentleader[0]) {
				if len(commentleader) == 1 {
					mode = INCOMMENT
				} else {
					c, err = getachar()
					if err == nil && c == commentleader[1] {
						mode = INCOMMENT
					}
				}
			} else if !isspace(c) {
				sawchar = true
			}
		} else if mode == INSTRING {
			if c == delimseen {
				mode = NORMAL
			} else if !isspace(c) {
				sawchar = true
			}
		} else { /* INCOMMENT mode */
			if (c == '\n') {
				mode = NORMAL
			}
		}
		if c == '\n' {
			if sawchar {
				sloc++
			}
			sawchar = false
		}
	}

	/* We're done with the file.  Handle EOF-without-EOL. */
	if sawchar {
		sloc++
	}
	sawchar = false

	if mode == INCOMMENT {
		log.Printf("ERROR - terminated in comment in %s\n", path)
	} else if mode == INSTRING {
		log.Printf("ERROR - terminated in string in %s\n", path)
	}

	return sloc
}

// Generic - recognize lots of languages with generic syntax
func Generic(path string) SourceStat {
	var stat SourceStat

	type scriptingLanguage struct {
		name string
		suffix string
		hashbang string
		stringdelims string
		commentleader string
	}
	scriptingLanguages := []scriptingLanguage{
		// First line doesn't look like it handles Python
		// multiline string literals, but it actually does.
		// The delimiters for them are ''' """ which get seen
		// as an empty string followed by a string delimiter,
		// or the reverse of that. Interior lines of a
		// multiline literal get counted if they contain non-
		// whitespace.
		//
		// This is different from sloccount's behavior, which
		// doesn't count multiline literals if they start at the
		// beginning of a line (e.g. as in Python header comments).
		{"Python", ".py", "python", "'\"", "#"},
		{"waf", "wscript", "waf", "'\"", "#"},
		{"Perl", ".pl", "perl", "'\"", "#"},
		{"C-shell", ".csh", "csh", "'\"", "#"},
		{"shell", ".sh", "sh", "'\"", "#"},
		{"Ruby", ".rb", "ruby", "'\"", "#"},
		{"Awk", ".awk", "awk", "'\"", "#"},
	}

	for i := range scriptingLanguages {
		lang := scriptingLanguages[i]
		if strings.HasSuffix(path, lang.suffix) || hashbang(path, lang.hashbang) {
			stat.Language = lang.name
			stat.SLOC = generic_sloc_count(path,
				lang.stringdelims, lang.commentleader)
			break
		}
	}

	type genericLanguage struct {
		name string
		suffix string
		commentleader string
	}
	genericLanguages := []genericLanguage{
		{"Ada", ".ada", "--"},
		{"Makefile", ".mk", "#"},
		{"Makefile", "Makefile", "#"},
		{"Makefile", "makefile", "#"},
		{"Lisp", ".lisp", ";"},
		{"Lisp", ".lsp", ";"},	// XLISP
		{"Lisp", ".cl", ";"},	// Common Lisp
	}

	for i := range genericLanguages {
		lang := genericLanguages[i]
		if strings.HasSuffix(path, lang.suffix) {
			stat.Language = lang.name
			stat.SLOC = generic_sloc_count(path,
				"", lang.commentleader)
			break
		}
	}

	return stat
}

// process - stub, eventually the statistics gatherer
func process(path string) {
	handlerList := []func(string) SourceStat {
		C,        /* also C++ */
		Generic,   /* Python, Perl, Ruby, shell, waf, Ada... */
	}
	var st SourceStat
	for i := range handlerList {
		st = handlerList[i](path)
		if st.SLOC > 0 {
			if !unclassified {
				fmt.Printf("%s %d %s\n", path, st.SLOC, st.Language)
			}
			return
		}
	}
	// Not a recognized source type, nor anything we know to discard
	if unclassified {
		fmt.Println(path)
	}
}

func isDirectory(path string) (bool) {
	fileInfo, err := os.Stat(path)
	return err == nil && fileInfo.IsDir()
}

// filter - winnows out uninteresting paths before handing them to process
func filter(path string, info os.FileInfo, err error) error {
	neverInterestingByPrefix := []string{"."}
	neverInterestingByInfix := []string{".so.", "/."}
	neverInterestingBySuffix := []string{"~",
		".a", ".la", ".o", ".so",
		".gif", ".jpg", ".jpeg", ".ico",
		".pyc", ".pyo"}

	for i := range neverInterestingByPrefix {
		if strings.HasPrefix(path, neverInterestingByPrefix[i]) {
			return err
		}
	}
	for i := range neverInterestingByInfix {
		if strings.Contains(path, neverInterestingByInfix[i]) {
			if isDirectory(path) {
				return filepath.SkipDir
			} else {
				return err
			}
		}
	}
	for i := range neverInterestingBySuffix {
		if strings.HasSuffix(path, neverInterestingBySuffix[i]) {
			return err
		}
	}
	for i := range exclusions {
		if path == exclusions[i] || strings.HasPrefix(path, exclusions[i]+"/") {
			return err
		}
	}

	/* has to come after the infix check for directory */
	if isDirectory(path) {
		return err
	}
	
	process(path)
	return err
}

func main() {
	excludePtr := flag.String("exclude", "",
		"paths directories to exclude")
	flag.BoolVar(&unclassified, "unclassified", false,
		"list unclassified files")
	flag.Parse()

	exclusions = strings.Split(*excludePtr, ",")
	roots := flag.Args()
	for i := range roots {
		filepath.Walk(roots[i], filter)
	}
}

// end

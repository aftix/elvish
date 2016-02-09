// Package eval handles evaluation of nodes and consists the runtime of the
// shell.
package eval

import (
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/elves/elvish/errutil"
	"github.com/elves/elvish/logutil"
	"github.com/elves/elvish/parse"
	"github.com/elves/elvish/store"
)

var Logger = logutil.Discard

// FnPrefix is the prefix for the variable names of functions. Defining a
// function "foo" is equivalent to setting a variable named FnPrefix + "foo".
const FnPrefix = "&"

// ns is a namespace.
type ns map[string]Variable

// Evaler is used to evaluate elvish sources. It maintains runtime context
// shared among all evalCtx instances.
type Evaler struct {
	global      ns
	mod         map[string]ns
	searchPaths []string
	store       *store.Store
	Editor      Editor
}

// evalCtx maintains an Evaler along with its runtime context. After creation
// an evalCtx is not modified, and new instances are created when needed.
type evalCtx struct {
	*Evaler
	name, text, context string

	local, up ns
	ports     []*Port
}

// NewEvaler creates a new Evaler.
func NewEvaler(st *store.Store) *Evaler {
	// Construct searchPaths
	var searchPaths []string
	if path := os.Getenv("PATH"); path != "" {
		searchPaths = strings.Split(path, ":")
	} else {
		searchPaths = []string{"/bin"}
	}

	// Construct initial global namespace
	pid := String(strconv.Itoa(syscall.Getpid()))
	paths := NewList()
	paths.appendStrings(searchPaths)
	global := ns{
		"pid":   newPtrVariable(pid),
		"ok":    newPtrVariable(OK),
		"true":  newPtrVariable(Bool(true)),
		"false": newPtrVariable(Bool(false)),
		"paths": newPtrVariable(paths),
	}
	for _, b := range builtinFns {
		global[FnPrefix+b.Name] = newPtrVariable(b)
	}

	return &Evaler{global, map[string]ns{}, searchPaths, st, nil}
}

// PprintError pretty prints an error. It understands specialized error types
// defined in this package.
func PprintError(e error) {
	switch e := e.(type) {
	case nil:
		fmt.Print("\033[32mok\033[m")
	case multiError:
		fmt.Print("(")
		for i, c := range e.errors {
			if i > 0 {
				fmt.Print(" | ")
			}
			PprintError(c.inner)
		}
		fmt.Print(")")
	case flow:
		fmt.Print("\033[33m" + e.Error() + "\033[m")
	default:
		fmt.Print("\033[31;1m" + e.Error() + "\033[m")
	}
}

const (
	outChanSize   = 32
	outChanLeader = "▶ "
)

// NewTopEvalCtx creates a top-level evalCtx.
func NewTopEvalCtx(ev *Evaler, name, text string, ports []*Port) *evalCtx {
	return &evalCtx{
		ev,
		name, text, "top",
		ev.global, ns{},
		ports,
	}
}

// fork returns a modified copy of ec. The ports are copied deeply, with
// shouldClose flags reset, and the context is changed to the given value.
// Other fields are copied shallowly.
func (ec *evalCtx) fork(newContext string) *evalCtx {
	newPorts := make([]*Port, len(ec.ports))
	for i, p := range ec.ports {
		newPorts[i] = &Port{p.File, p.Chan, false, false}
	}
	return &evalCtx{
		ec.Evaler,
		ec.name, ec.text, newContext,
		ec.local, ec.up,
		newPorts,
	}
}

// port returns ec.ports[i] or nil if i is out of range. This makes it possible
// to treat ec.ports as if it has an infinite tail of nil's.
func (ec *evalCtx) port(i int) *Port {
	if i >= len(ec.ports) {
		return nil
	}
	return ec.ports[i]
}

// growPorts makes the size of ec.ports at least n, adding nil's if necessary.
func (ec *evalCtx) growPorts(n int) {
	if len(ec.ports) >= n {
		return
	}
	ports := ec.ports
	ec.ports = make([]*Port, n)
	copy(ec.ports, ports)
}

func makeScope(s ns) scope {
	sc := scope{}
	for name := range s {
		sc[name] = true
	}
	return sc
}

// Eval evaluates a chunk node n. The supplied name and text are used in
// diagnostic messages.
func (ev *Evaler) Eval(name, text string, n *parse.Chunk, ports []*Port) error {
	op, err := ev.Compile(name, text, n)
	if err != nil {
		return err
	}
	ec := NewTopEvalCtx(ev, name, text, ports)
	return ec.PEval(op)
}

func (ev *Evaler) EvalInteractive(text string, n *parse.Chunk) error {
	outCh := make(chan Value, outChanSize)
	outDone := make(chan struct{})
	go func() {
		for v := range outCh {
			fmt.Printf("%s%s\n", outChanLeader, v.Repr())
		}
		close(outDone)
	}()

	ports := []*Port{
		{File: os.Stdin},
		{File: os.Stdout, Chan: outCh},
		{File: os.Stderr},
	}

	err := ev.Eval("[interactive]", text, n, ports)
	close(outCh)
	<-outDone
	return err
}

// Compile compiles elvish code in the global scope.
func (ev *Evaler) Compile(name, text string, n *parse.Chunk) (Op, error) {
	return compile(name, text, makeScope(ev.global), n)
}

// PEval evaluates an op in a protected environment so that calls to errorf are
// wrapped in an Error.
func (ec *evalCtx) PEval(op Op) (ex error) {
	defer errutil.Catch(&ex)
	op(ec)
	return nil
}

// errorf stops the ec.eval immediately by panicking with a diagnostic message.
// The panic is supposed to be caught by ec.eval.
func (ec *evalCtx) errorf(p int, format string, args ...interface{}) {
	throw(errutil.NewContextualError(
		fmt.Sprintf("%s (%s)", ec.name, ec.context), "error",
		ec.text, p, format, args...))
}

// mustSingleString returns a String if that is the only element of vs.
// Otherwise it errors.
func (ec *evalCtx) mustSingleString(vs []Value, what string, p int) String {
	if len(vs) != 1 {
		ec.errorf(p, "Expect exactly one word for %s, got %d", what, len(vs))
	}
	v, ok := vs[0].(String)
	if !ok {
		ec.errorf(p, "Expect string for %s, got %s", what, vs[0])
	}
	return v
}

// SourceText evaluates a chunk of elvish source.
func (ev *Evaler) SourceText(src string) error {
	n, err := parse.Parse(src)
	if err != nil {
		return err
	}
	return ev.EvalInteractive(src, n)
}

func readFileUTF8(fname string) (string, error) {
	bytes, err := ioutil.ReadFile(fname)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(bytes) {
		return "", fmt.Errorf("%s: source is not valid UTF-8", fname)
	}
	return string(bytes), nil
}

// Source evaluates the content of a file.
func (ev *Evaler) Source(fname string) error {
	src, err := readFileUTF8(fname)
	if err != nil {
		return err
	}
	return ev.SourceText(src)
}

// Global returns the global namespace.
func (ev *Evaler) Global() map[string]Variable {
	return map[string]Variable(ev.global)
}

// ResolveVar resolves a variable. When the variable cannot be found, nil is
// returned.
func (ec *evalCtx) ResolveVar(ns, name string) Variable {
	if ns == "env" {
		return newEnvVariable(name)
	}
	if mod, ok := ec.mod[ns]; ok {
		return mod[name]
	}

	may := func(n string) bool {
		return ns == "" || ns == n
	}
	if may("local") {
		if v, ok := ec.local[name]; ok {
			return v
		}
	}
	if may("up") {
		if v, ok := ec.up[name]; ok {
			return v
		}
	}
	return nil
}

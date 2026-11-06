package main

// `draftcat gate install`: writes the `draftcat gate hook` entry into an agent
// harness's own settings file, next to whatever else the file holds.
//
// The entry is found again by its command (it always contains
// hookCommandMarker), so re-running replaces it and --remove takes out only
// that entry. Key order, other keys and other hooks are kept; the file is
// replaced atomically. The installed command reads the gate URL and token
// from the harness's environment, so nothing secret is written.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// hookCommandMarker is in every command the installer writes and in no other.
const hookCommandMarker = " gate hook --format "

// hookTarget is one harness: where its settings live, how a hook entry looks,
// which event fires before a tool call, and which tools the default matcher
// sends to the gate.
type hookTarget struct {
	name    string
	format  string // `gate hook --format`
	event   string
	project string // settings file under the project directory
	userEnv string // optional variable that relocates the user-level directory
	userDir string // user-level directory under the home directory
	userRel string // settings file under the user-level directory
	matcher string // default matcher
	// tools are the names the harness reports for its built-in tools, for the
	// install notice; mcp is how it names an MCP tool.
	tools []string
	mcp   string
	// nonZeroBlocks: the harness denies the call when a hook exits non-zero, so
	// the command needs no shell fallback (and stays valid in PowerShell).
	nonZeroBlocks bool
	// anchoredRegex: a matcher that is not a plain name list is a regex
	// anchored at both ends; otherwise it matches anywhere in the name.
	anchoredRegex bool
	// regexOnly: every matcher is a regex, including a plain name.
	regexOnly bool
	entry     func(command, matcher string, timeout time.Duration) *jsonObj
}

var hookTargets = []hookTarget{
	{
		name: "claude-code", format: "claude", event: "PreToolUse",
		project: filepath.Join(".claude", "settings.json"),
		userDir: ".claude", userRel: "settings.json",
		matcher: "^(Bash|Write|Edit|MultiEdit|NotebookEdit|mcp__.*)$",
		tools:   []string{"Bash", "Write", "Edit", "MultiEdit", "NotebookEdit", "Read", "Glob", "Grep", "WebFetch", "WebSearch", "Task", "TodoWrite"},
		mcp:     "mcp__<server>__<tool>",
		entry: func(command, matcher string, timeout time.Duration) *jsonObj {
			return newJSONObj("matcher", matcher, "hooks", []interface{}{
				newJSONObj("type", "command", "command", command, "timeout", jsonSeconds(timeout)),
			})
		},
	},
	{
		name: "codex", format: "codex", event: "PreToolUse",
		project: filepath.Join(".codex", "hooks.json"),
		userEnv: "CODEX_HOME", userDir: ".codex", userRel: "hooks.json",
		matcher: "^(Bash|apply_patch|mcp__.*)$",
		tools:   []string{"Bash", "apply_patch"},
		mcp:     "mcp__<server>__<tool>",
		entry: func(command, matcher string, timeout time.Duration) *jsonObj {
			return newJSONObj("matcher", matcher, "hooks", []interface{}{
				newJSONObj("type", "command", "command", command, "timeout", jsonSeconds(timeout),
					"statusMessage", "draftcat: waiting for the tool gate"),
			})
		},
	},
	{
		name: "copilot-cli", format: "copilot", event: "PreToolUse",
		project: filepath.Join(".github", "hooks", "draftcat.json"),
		userEnv: "COPILOT_HOME", userDir: ".copilot", userRel: filepath.Join("hooks", "draftcat.json"),
		// Copilot CLI reports MCP tools as <server>-<tool> with no common
		// prefix, so they are added to the matcher by name.
		matcher:       "Bash|Write|Edit",
		tools:         []string{"Bash", "Write", "Edit", "Read", "Grep", "Glob", "WebFetch", "WebSearch", "AskUserQuestion", "TodoWrite", "Agent"},
		nonZeroBlocks: true, anchoredRegex: true,
		entry: func(command, matcher string, timeout time.Duration) *jsonObj {
			return newJSONObj("type", "command", "matcher", matcher, "command", command, "timeoutSec", jsonSeconds(timeout))
		},
	},
	{
		name: "gemini-cli", format: "gemini", event: "BeforeTool",
		project: filepath.Join(".gemini", "settings.json"),
		userDir: ".gemini", userRel: "settings.json",
		matcher:   "^(run_shell_command|write_file|replace|mcp_.*)$",
		tools:     []string{"run_shell_command", "write_file", "replace", "read_file", "read_many_files", "list_directory", "glob", "search_file_content", "web_fetch", "google_web_search", "save_memory", "write_todos"},
		mcp:       "mcp_<server>_<tool>",
		regexOnly: true,
		entry: func(command, matcher string, timeout time.Duration) *jsonObj {
			return newJSONObj("matcher", matcher, "hooks", []interface{}{
				newJSONObj("name", "draftcat-gate", "type", "command", "command", command,
					"timeout", json.Number(strconv.FormatInt(timeout.Milliseconds(), 10)),
					"description", "asks the draftcat tool gate before the call runs"),
			})
		},
	},
}

func jsonSeconds(d time.Duration) json.Number {
	return json.Number(strconv.FormatInt(int64((d+time.Second-1)/time.Second), 10))
}

func lookupHookTarget(name string) (hookTarget, bool) {
	for _, t := range hookTargets {
		if t.name == name {
			return t, true
		}
	}
	return hookTarget{}, false
}

const gateInstallUsage = `Usage:
  draftcat gate install claude-code|codex|copilot-cli|gemini-cli [--matcher REGEX] [--dir DIR | --user]
                        [--agent NAME] [--token-env VAR] [--wait 5m] [--bin PATH] [--dry-run] [--remove]

Writes the draftcat gate hook into the harness's settings file, keeping every
other key and hook: .claude/settings.json, .codex/hooks.json,
.github/hooks/draftcat.json or .gemini/settings.json under --dir (default the
current directory), or the home-directory file with --user. Re-running replaces
draftcat's entry; --remove takes only that entry out; --dry-run prints the
resulting file instead of writing it.

--matcher    which tool calls fire the hook (default: shell, file-writing and MCP tools)
--agent, --token-env, --wait   passed to 'draftcat gate hook' in the installed command
--bin        the draftcat executable the command runs (default: draftcat on PATH)

The installed command reads the gate URL and token from the harness's
environment (DRAFTCAT_GATE_URL, DRAFTCAT_GATE_TOKEN); nothing secret is written.`

type installOptions struct {
	matcher  string
	dir      string
	user     bool
	dryRun   bool
	remove   bool
	agent    string
	tokenEnv string
	wait     time.Duration
	bin      string
}

func gateInstallCmd(args []string, stdout, stderr io.Writer) int {
	var o installOptions
	fs := flag.NewFlagSet("gate install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.matcher, "matcher", "", "")
	fs.StringVar(&o.dir, "dir", "", "")
	fs.BoolVar(&o.user, "user", false, "")
	fs.BoolVar(&o.dryRun, "dry-run", false, "")
	fs.BoolVar(&o.remove, "remove", false, "")
	fs.StringVar(&o.agent, "agent", "", "")
	fs.StringVar(&o.tokenEnv, "token-env", "", "")
	fs.DurationVar(&o.wait, "wait", hookDefaultWait, "")
	fs.StringVar(&o.bin, "bin", "draftcat", "")
	// The target may come before or after the flags.
	var positional []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				_, _ = fmt.Fprintln(stdout, gateInstallUsage)
				return 0
			}
			_, _ = fmt.Fprintf(stderr, "gate install: %v\n%s\n", err, gateInstallUsage)
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(positional) != 1 {
		_, _ = fmt.Fprintln(stderr, gateInstallUsage)
		return 2
	}
	t, ok := lookupHookTarget(positional[0])
	if !ok {
		_, _ = fmt.Fprintf(stderr, "gate install: unknown harness %q (claude-code, codex, copilot-cli or gemini-cli)\n", positional[0])
		return 2
	}
	if o.user && o.dir != "" {
		_, _ = fmt.Fprintln(stderr, "gate install: --user and --dir are exclusive")
		return 2
	}
	if o.wait <= 0 {
		_, _ = fmt.Fprintln(stderr, "gate install: --wait must be a positive duration")
		return 2
	}
	if strings.TrimSpace(o.bin) == "" {
		_, _ = fmt.Fprintln(stderr, "gate install: --bin must name an executable")
		return 2
	}
	if o.matcher == "" {
		o.matcher = t.matcher
	}
	path, err := hookSettingsPath(t, o)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gate install: %v\n", err)
		return 1
	}
	res, err := installHook(t, path, o)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gate install: %v\n", err)
		return 1
	}
	notes := stdout
	if o.dryRun {
		_, _ = stdout.Write(res.content)
		notes = stderr
	}
	printInstallNotes(notes, t, o, res)
	return 0
}

// hookSettingsPath is the settings file draftcat's entry goes into.
func hookSettingsPath(t hookTarget, o installOptions) (string, error) {
	if !o.user {
		dir := o.dir
		if dir == "" {
			dir = "."
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", err
		}
		return filepath.Join(abs, t.project), nil
	}
	if t.userEnv != "" {
		if d := strings.TrimSpace(os.Getenv(t.userEnv)); d != "" {
			return filepath.Join(d, t.userRel), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory for --user: %w", err)
	}
	return filepath.Join(home, t.userDir, t.userRel), nil
}

// hookCommand is the shell command the harness runs. Where a hook that exits
// with anything but 2 (or a JSON deny) lets the call through, a command that
// cannot run at all still exits 2.
func hookCommand(t hookTarget, o installOptions) string {
	parts := []string{shellQuote(o.bin), "gate", "hook", "--format", t.format}
	if o.agent != "" {
		parts = append(parts, "--agent", shellQuote(o.agent))
	}
	if o.tokenEnv != "" {
		parts = append(parts, "--token-env", shellQuote(o.tokenEnv))
	}
	if o.wait != hookDefaultWait {
		parts = append(parts, "--wait", o.wait.String())
	}
	cmd := strings.Join(parts, " ")
	if !t.nonZeroBlocks {
		cmd += ` || { echo 'draftcat gate hook could not run; the call is blocked' >&2; exit 2; }`
	}
	return cmd
}

// hookTimeout is the harness's own timeout for the hook: a minute past the
// hook's wait, so the hook answers before the harness gives up on it.
func hookTimeout(wait time.Duration) time.Duration { return wait + time.Minute }

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:=@+-]+$`)

func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type installResult struct {
	path    string
	outcome string // added | updated | unchanged | removed | absent
	content []byte
}

// installHook upserts (or removes) draftcat's entry in one settings file.
func installHook(t hookTarget, path string, o installOptions) (installResult, error) {
	res := installResult{path: path}
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved // keep a symlinked settings file a symlink
	}
	// #nosec G304 -- the harness settings file the operator named.
	raw, err := os.ReadFile(target)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return res, err
	}
	root := newJSONObj()
	if len(bytes.TrimSpace(raw)) > 0 {
		root, err = decodeOrderedObject(raw)
		if err != nil {
			return res, fmt.Errorf("cannot update %s: %w", path, err)
		}
	}
	before := encodeOrderedJSON(root)
	if t.name == "copilot-cli" && !o.remove {
		if _, ok := root.get("version"); !ok {
			root.set("version", json.Number("1"))
		}
	}
	var entry *jsonObj
	if !o.remove {
		entry = t.entry(hookCommand(t, o), o.matcher, hookTimeout(o.wait))
	}
	found, err := upsertHookEntry(root, t.event, entry)
	if err != nil {
		return res, fmt.Errorf("cannot update %s: %w", path, err)
	}
	res.content = encodeOrderedJSON(root)
	changed := !bytes.Equal(before, res.content) || (!exists && !o.remove)
	switch {
	case o.remove && found:
		res.outcome = "removed"
	case o.remove:
		res.outcome = "absent"
	case !found:
		res.outcome = "added"
	case changed:
		res.outcome = "updated"
	default:
		res.outcome = "unchanged"
	}
	if o.dryRun || !changed || (o.remove && !exists) {
		return res, nil
	}
	return res, writeFileAtomic(target, res.content)
}

// upsertHookEntry takes draftcat's entries out of hooks.<event> and, when
// entry is not nil, puts entry where the first one was (or at the end). It
// reports whether an entry of draftcat's was there.
func upsertHookEntry(root *jsonObj, event string, entry *jsonObj) (bool, error) {
	hv, ok := root.get("hooks")
	if !ok {
		if entry == nil {
			return false, nil
		}
		hv = newJSONObj()
		root.set("hooks", hv)
	}
	hooks, ok := hv.(*jsonObj)
	if !ok {
		return false, errors.New("hooks is not an object")
	}
	lv, had := hooks.get(event)
	if !had {
		lv = []interface{}{}
	}
	list, isList := lv.([]interface{})
	if !isList {
		return false, fmt.Errorf("hooks.%s is not a list", event)
	}
	found, insertAt := false, -1
	kept := make([]interface{}, 0, len(list)+1)
	for _, item := range list {
		obj, isObj := item.(*jsonObj)
		if !isObj {
			kept = append(kept, item)
			continue
		}
		if isDraftcatHook(obj) {
			found = true
			if insertAt < 0 {
				insertAt = len(kept)
			}
			continue
		}
		// A matcher group: drop draftcat's commands from it, and the group
		// itself only when nothing else is left in it.
		inner, isGroup := obj.get("hooks")
		innerList, isList := inner.([]interface{})
		if !isGroup || !isList {
			kept = append(kept, item)
			continue
		}
		rest := make([]interface{}, 0, len(innerList))
		for _, h := range innerList {
			if ho, ok := h.(*jsonObj); ok && isDraftcatHook(ho) {
				found = true
				continue
			}
			rest = append(rest, h)
		}
		if len(rest) == 0 && len(innerList) > 0 {
			if insertAt < 0 {
				insertAt = len(kept)
			}
			continue
		}
		if len(rest) != len(innerList) {
			obj.set("hooks", rest)
		}
		kept = append(kept, obj)
	}
	if entry == nil {
		if !found {
			return false, nil
		}
		// Only what draftcat's entry leaves empty is taken out with it.
		if len(kept) > 0 {
			hooks.set(event, kept)
			return true, nil
		}
		hooks.del(event)
		if len(hooks.keys) == 0 {
			root.del("hooks")
		}
		return true, nil
	}
	if insertAt < 0 {
		insertAt = len(kept)
	}
	kept = append(kept[:insertAt], append([]interface{}{entry}, kept[insertAt:]...)...)
	hooks.set(event, kept)
	return found, nil
}

// isDraftcatHook reports whether a hook object runs `draftcat gate hook`.
func isDraftcatHook(o *jsonObj) bool {
	for _, k := range []string{"command", "bash", "powershell"} {
		if v, ok := o.get(k); ok {
			if s, ok := v.(string); ok && strings.Contains(s, hookCommandMarker) {
				return true
			}
		}
	}
	return false
}

// writeFileAtomic replaces path with data through a temporary file in the
// same directory, keeping the old file's permissions.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	// #nosec G301 -- a project or home settings directory the harness reads.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".draftcat-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// matcherSends reports whether a tool name fires the hook under the harness's
// matcher rules: "" and "*" match everything, a plain name list matches
// names exactly, and anything else is a regular expression.
func matcherSends(t hookTarget, matcher, name string) (bool, error) {
	if matcher == "" || matcher == "*" || (t.anchoredRegex && matcher == "**") {
		return true, nil
	}
	plain := regexp.MustCompile(`^[A-Za-z0-9_|]+$`).MatchString(matcher)
	if plain && !t.regexOnly {
		for _, n := range strings.Split(matcher, "|") {
			if n == name {
				return true, nil
			}
		}
		return false, nil
	}
	expr := matcher
	if t.anchoredRegex {
		expr = "^(?:" + matcher + ")$"
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return false, err
	}
	return re.MatchString(name), nil
}

// sentToolNames lists the tool names the matcher sends to the gate, of the
// names the harness is known to report, plus its MCP naming when MCP tools
// match.
func sentToolNames(t hookTarget, matcher string) ([]string, error) {
	var names []string
	for _, n := range t.tools {
		ok, err := matcherSends(t, matcher, n)
		if err != nil {
			return nil, err
		}
		if ok {
			names = append(names, n)
		}
	}
	if t.mcp != "" {
		sample := strings.NewReplacer("<server>", "server", "<tool>", "tool").Replace(t.mcp)
		if ok, _ := matcherSends(t, matcher, sample); ok {
			names = append(names, t.mcp)
		}
	}
	return names, nil
}

func printInstallNotes(w io.Writer, t hookTarget, o installOptions, res installResult) {
	switch res.outcome {
	case "removed":
		_, _ = fmt.Fprintf(w, "removed the draftcat hook from %s\n", res.path)
		return
	case "absent":
		_, _ = fmt.Fprintf(w, "no draftcat hook in %s\n", res.path)
		return
	}
	verb := map[string]string{"added": "added", "updated": "updated", "unchanged": "already installed:"}[res.outcome]
	if o.dryRun {
		verb = "would write"
	}
	_, _ = fmt.Fprintf(w, "%s the draftcat %s hook in %s (matcher %s)\n", verb, t.event, res.path, o.matcher)
	names, err := sentToolNames(t, o.matcher)
	switch {
	case err != nil:
		_, _ = fmt.Fprintf(w, "the matcher is not a regular expression draftcat can read (%v); check it in the harness\n", err)
	case len(names) == 0:
		_, _ = fmt.Fprintln(w, "the matcher sends none of the harness's known tools to the gate")
	default:
		_, _ = fmt.Fprintf(w, "tool names the hook sends to the gate: %s\n", strings.Join(names, ", "))
	}
	if t.name == "copilot-cli" {
		_, _ = fmt.Fprintln(w, "Copilot CLI names MCP tools <server>-<tool>; add the ones to gate to --matcher")
	}
	_, _ = fmt.Fprintln(w, "the gate denies every tool not in tool_gate.tools: add a rule for each name it should decide")
	tokenEnv := o.tokenEnv
	if tokenEnv == "" {
		tokenEnv = hookDefaultTokenEnv
	}
	_, _ = fmt.Fprintf(w, "the harness's environment needs %s (the gate token) and %s if the gate is not at %s\n", tokenEnv, hookURLEnv, hookDefaultURL)
	if t.name == "codex" && !o.user {
		_, _ = fmt.Fprintln(w, "Codex runs a new or changed project hook only after you review and trust it")
	}
}

// jsonObj is a JSON object that keeps its key order, so a settings file
// comes back with its keys where they were.
type jsonObj struct {
	keys []string
	vals map[string]interface{}
}

// newJSONObj builds an object from key, value pairs.
func newJSONObj(kv ...interface{}) *jsonObj {
	o := &jsonObj{vals: map[string]interface{}{}}
	for i := 0; i+1 < len(kv); i += 2 {
		k, _ := kv[i].(string)
		o.set(k, kv[i+1])
	}
	return o
}

func (o *jsonObj) get(k string) (interface{}, bool) {
	v, ok := o.vals[k]
	return v, ok
}

func (o *jsonObj) set(k string, v interface{}) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *jsonObj) del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, key := range o.keys {
		if key == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// decodeOrderedObject reads a JSON object, keeping key order and number
// spelling. Anything but one object (a JSONC comment, say) is an error.
func decodeOrderedObject(raw []byte) (*jsonObj, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeOrderedValue(dec, 0)
	if err != nil {
		return nil, fmt.Errorf("not a JSON object: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("not a JSON object: content after the object")
	}
	o, ok := v.(*jsonObj)
	if !ok {
		return nil, errors.New("not a JSON object")
	}
	return o, nil
}

func decodeOrderedValue(dec *json.Decoder, depth int) (interface{}, error) {
	if depth > 128 {
		return nil, errors.New("nesting exceeds 128 levels")
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, isDelim := tok.(json.Delim)
	if !isDelim {
		return tok, nil
	}
	switch d {
	case '{':
		o := newJSONObj()
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k, _ := kt.(string)
			v, err := decodeOrderedValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			o.set(k, v)
		}
		_, err := dec.Token()
		return o, err
	case '[':
		list := []interface{}{}
		for dec.More() {
			v, err := decodeOrderedValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		_, err := dec.Token()
		return list, err
	}
	return nil, fmt.Errorf("unexpected %v", d)
}

// encodeOrderedJSON writes v with two-space indentation and a final newline,
// without escaping <, > and & (a shell command reads better as written).
func encodeOrderedJSON(v interface{}) []byte {
	var b bytes.Buffer
	writeOrderedJSON(&b, v, 0)
	b.WriteByte('\n')
	return b.Bytes()
}

func writeOrderedJSON(b *bytes.Buffer, v interface{}, depth int) {
	indent := func(n int) {
		b.WriteByte('\n')
		b.WriteString(strings.Repeat("  ", n))
	}
	switch x := v.(type) {
	case *jsonObj:
		if len(x.keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteByte('{')
		for i, k := range x.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			indent(depth + 1)
			writeJSONScalar(b, k)
			b.WriteString(": ")
			writeOrderedJSON(b, x.vals[k], depth+1)
		}
		indent(depth)
		b.WriteByte('}')
	case []interface{}:
		if len(x) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			indent(depth + 1)
			writeOrderedJSON(b, e, depth+1)
		}
		indent(depth)
		b.WriteByte(']')
	default:
		writeJSONScalar(b, x)
	}
}

func writeJSONScalar(b *bytes.Buffer, v interface{}) {
	var s bytes.Buffer
	enc := json.NewEncoder(&s)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		b.WriteString("null")
		return
	}
	b.Write(bytes.TrimRight(s.Bytes(), "\n"))
}

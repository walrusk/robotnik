package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"unicode"
)

const codexAICommand = `out=$(mktemp); log=$(mktemp); if codex exec --skip-git-repo-check --ephemeral --sandbox read-only --color never -c approval_policy="never" -c model_reasoning_effort="medium" --output-last-message "$out" - >"$log" 2>&1; then cat "$out"; rc=0; else rc=$?; cat "$log" >&2; fi; rm -f "$out" "$log"; exit $rc`
const claudeAICommand = `claude -p --no-session-persistence --permission-mode dontAsk --tools "" --output-format text`
const defaultAICommand = codexAICommand

const systemPrompt = `You convert a natural-language request into a short menu of Bash command options.

Return JSON only, with this shape:
{"options":[{"title":"short label","command":"single-line bash command","notes":"brief caveat"}]}

Rules:
- Produce exactly 2 options.
- Every command must be shown in full and fit on one shell line.
- Do not run shell commands, inspect files, or modify the workspace. Only return JSON.
- Prefer commands that run on macOS and Linux Bash.
- Prefer read-only preview commands first when the request might mutate, delete, overwrite, publish, install, or expose data.
- Avoid sudo unless the user explicitly requested system-level changes.
- Avoid curl/wget piped into a shell.
- Avoid commands that read .env, .env.*, .envrc, private keys, tokens, credentials, or browser profiles.
- If the request is ambiguous, include a read-only inspection command rather than guessing destructively.
- Do not wrap commands in markdown fences.
- Do not include risk labels; the wrapper will evaluate risk independently.
`

type option struct {
	Title   string
	Command string
	Notes   string
}

var (
	curlPipeShellRE = regexp.MustCompile(`(curl|wget)[^|]*\|[[:space:]]*(sh|bash)`)
	evalRE          = regexp.MustCompile(`(^|[[:space:];|&])eval[[:space:]]`)
	sudoRE          = regexp.MustCompile(`(^|[[:space:];|&])sudo[[:space:]]`)
	zeroRiskRE      = regexp.MustCompile(`^[[:space:]]*(pwd|date|whoami|true|false)[[:space:]]*$`)
	echoRiskRE      = regexp.MustCompile(`^[[:space:]]*(echo|printf)[[:space:]]`)
	lowRiskRE       = regexp.MustCompile(`(^|[[:space:];|&])(ls|find|grep|rg|cat|head|tail|wc|awk|git[[:space:]]+status|git[[:space:]]+diff|git[[:space:]]+log|git[[:space:]]+branch|git[[:space:]]+rev-parse)([[:space:];|&]|$)`)
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	allowMax, color, remaining, showHelp, err := parseArgs(args)
	if err != nil {
		die(err.Error())
		return 1
	}

	if showHelp {
		usage(os.Stderr)
		return 0
	}

	if len(remaining) == 0 {
		usage(os.Stderr)
		return 1
	}

	if !isTerminal(os.Stdout) || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		color = false
	}

	prompt := strings.Join(remaining, " ")

	aiCommand, err := resolveAICommand(os.Stdin, os.Stderr)
	if err != nil {
		die(err.Error())
		return 1
	}
	_ = os.Setenv("ROBOTNIK_AI_CMD", aiCommand)

	fmt.Fprintln(os.Stderr, "Thinking...")
	raw, exitCode, err := generateWithAICommand(prompt, aiCommand, os.Stderr)
	if err != nil {
		if exitCode >= 0 {
			return exitCode
		}
		die(err.Error())
		return 1
	}

	options, err := normalizeOptions(raw)
	if err != nil {
		die(err.Error())
		return 1
	}

	risks := make([]string, len(options))

	fmt.Fprintln(os.Stdout)
	fmt.Fprintln(os.Stdout, "Options:")
	for i, opt := range options {
		risk := riskForCommand(opt.Command)
		risks[i] = risk
		fmt.Fprintf(os.Stdout, "%d. [%s risk] %s\n", i+1, colorRisk(risk, color), opt.Command)
	}

	choice, err := readChoice(os.Stdin, os.Stdout, len(options))
	if err != nil {
		die(err.Error())
		return 1
	}

	if choice == "q" || choice == "Q" || choice == "" {
		fmt.Fprintln(os.Stdout, "Cancelled.")
		return 0
	}

	selection, err := strconv.Atoi(choice)
	if err != nil || selection < 1 || selection > len(options) {
		die("invalid selection: " + choice)
		return 1
	}

	selectedIndex := selection - 1
	selectedRisk := risks[selectedIndex]
	selectedCommand := options[selectedIndex].Command

	if selectedRisk == "max" && !allowMax {
		die("refusing to run max-risk command without --allow-max")
		return 1
	}

	fmt.Fprintln(os.Stdout)
	fmt.Fprintf(os.Stdout, "Running [%s]: %s\n", colorRisk(selectedRisk, color), selectedCommand)
	return runBashCommand(selectedCommand)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  robotnik [--allow-max] [--no-color] <natural language shell request>

Examples:
  robotnik delete all branches except for the current one and main
  robotnik show the largest files under this repo

Configuration:
  ROBOTNIK_AI_CMD            Optional custom generator command. If unset,
                             Robotnik asks you to choose Claude Code or Codex
                             on first run and saves the default to:
                             ${XDG_CONFIG_HOME:-$HOME/.config}/robotnik/config

                             The command receives the full prompt on stdin and must print JSON:
                             {"options":[{"title":"","command":"","notes":""}]}.

Safety:
  Robotnik asks AI for command candidates, then independently assigns a local risk label.
  Selecting a max-risk command is refused unless --allow-max is passed.
`)
}

func die(message string) {
	fmt.Fprintln(os.Stderr, "robotnik: "+message)
}

func parseArgs(args []string) (allowMax bool, color bool, remaining []string, showHelp bool, err error) {
	color = true

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--allow-max":
			allowMax = true
		case "--no-color":
			color = false
		case "-h", "--help":
			showHelp = true
			return allowMax, color, nil, showHelp, nil
		case "--":
			return allowMax, color, args[i+1:], false, nil
		default:
			if strings.HasPrefix(args[i], "-") {
				return false, false, nil, false, fmt.Errorf("unknown option: %s", args[i])
			}
			return allowMax, color, args[i:], false, nil
		}
	}

	return allowMax, color, nil, false, nil
}

func generateWithAICommand(prompt, aiCommand string, stderr io.Writer) ([]byte, int, error) {
	generatorPrompt := systemPrompt + "\n" + requestPrompt(prompt)
	cmd := exec.Command("bash", "-lc", aiCommand)
	cmd.Stdin = strings.NewReader(generatorPrompt)
	cmd.Stderr = stderr

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return stdout.Bytes(), exitErr.ExitCode(), err
		}
		return stdout.Bytes(), -1, err
	}

	return stdout.Bytes(), 0, nil
}

func resolveAICommand(in *os.File, out io.Writer) (string, error) {
	if aiCommand := os.Getenv("ROBOTNIK_AI_CMD"); aiCommand != "" {
		return aiCommand, nil
	}

	configPath, err := robotnikConfigPath()
	if err != nil {
		if !isTerminal(in) {
			return defaultAICommand, nil
		}
		return "", err
	}

	if aiCommand, ok, err := readConfiguredAICommand(configPath); err != nil {
		return "", err
	} else if ok {
		return aiCommand, nil
	}

	if !isTerminal(in) {
		return defaultAICommand, nil
	}

	aiCommand, err := promptForAICommand(in, out)
	if err != nil {
		return "", err
	}

	if err := writeConfiguredAICommand(configPath, aiCommand); err != nil {
		return "", err
	}

	fmt.Fprintf(out, "Saved default AI command to %s\n\n", configPath)
	return aiCommand, nil
}

func robotnikConfigPath() (string, error) {
	if path := os.Getenv("ROBOTNIK_CONFIG"); path != "" {
		return path, nil
	}

	if configHome := os.Getenv("XDG_CONFIG_HOME"); configHome != "" {
		return filepath.Join(configHome, "robotnik", "config"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("could not find home directory for robotnik config")
	}

	return filepath.Join(home, ".config", "robotnik", "config"), nil
}

func readConfiguredAICommand(path string) (string, bool, error) {
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("could not read robotnik config: %w", err)
	}

	for _, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "export ")
		value, ok := strings.CutPrefix(line, "ROBOTNIK_AI_CMD=")
		if !ok {
			continue
		}

		value = strings.TrimSpace(value)
		value = unquoteConfigValue(value)
		if value == "" {
			return "", false, nil
		}
		return value, true, nil
	}

	return "", false, nil
}

func unquoteConfigValue(value string) string {
	if len(value) < 2 {
		return value
	}

	first := value[0]
	last := value[len(value)-1]
	if (first == '\'' && last == '\'') || (first == '"' && last == '"') {
		return value[1 : len(value)-1]
	}
	return value
}

func promptForAICommand(in *os.File, out io.Writer) (string, error) {
	reader := bufio.NewReader(in)

	fmt.Fprintln(out, "Choose your AI CLI for Robotnik:")
	fmt.Fprintln(out, "1. Claude Code")
	fmt.Fprintln(out, "2. Codex")

	for {
		fmt.Fprint(out, "Selection [1-2]: ")
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}

		choice := strings.TrimSpace(line)
		choice = strings.TrimSuffix(choice, ".")
		switch choice {
		case "1":
			return claudeAICommand, nil
		case "2":
			return codexAICommand, nil
		default:
			fmt.Fprintln(out, "Please enter 1 or 2.")
		}

		if errors.Is(err, io.EOF) {
			return "", errors.New("no AI CLI selected")
		}
	}
}

func writeConfiguredAICommand(path, aiCommand string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("could not create robotnik config directory: %w", err)
	}

	contents := "# Robotnik config\n" +
		"# Used when ROBOTNIK_AI_CMD is not set in the environment.\n" +
		"ROBOTNIK_AI_CMD=" + aiCommand + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		return fmt.Errorf("could not write robotnik config: %w", err)
	}

	return nil
}

func requestPrompt(prompt string) string {
	wd, err := os.Getwd()
	if err != nil {
		wd = ""
	}

	return fmt.Sprintf(`User request:
%s

Working directory:
%s

Shell:
bash

Operating system:
%s
`, prompt, wd, operatingSystem())
}

func operatingSystem() string {
	out, err := exec.Command("uname", "-s").Output()
	if err == nil {
		return strings.TrimSpace(string(out))
	}
	return runtime.GOOS
}

func normalizeOptions(raw []byte) ([]option, error) {
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return nil, errors.New("AI backend did not return valid JSON")
	}

	var rawOptions []any
	switch value := decoded.(type) {
	case []any:
		rawOptions = value
	case map[string]any:
		if options, ok := value["options"].([]any); ok {
			rawOptions = options
		}
	}

	optionsCapacity := len(rawOptions)
	if optionsCapacity > 9 {
		optionsCapacity = 9
	}
	options := make([]option, 0, optionsCapacity)
	for _, rawOption := range rawOptions {
		fields, ok := rawOption.(map[string]any)
		if !ok {
			continue
		}

		command := cleanCommand(firstField(fields, "", "command", "cmd"))
		if command == "" {
			continue
		}

		options = append(options, option{
			Title:   cleanText(firstField(fields, "Command", "title", "label", "description")),
			Command: command,
			Notes:   cleanText(firstField(fields, "", "notes", "explanation", "reason")),
		})
		if len(options) == 9 {
			break
		}
	}

	if len(options) == 0 {
		return nil, errors.New("AI backend did not return any command options")
	}

	return options, nil
}

func firstField(fields map[string]any, fallback string, names ...string) string {
	for _, name := range names {
		value, ok := fields[name]
		if !ok || value == nil {
			continue
		}
		return valueToString(value)
	}
	return fallback
}

func valueToString(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case json.Number:
		return value.String()
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Sprint(value)
		}
		return string(encoded)
	}
}

func cleanText(value string) string {
	return replaceRuns(value, func(r rune) bool {
		return r == '\r' || r == '\n' || r == '\t'
	})
}

func cleanCommand(value string) string {
	return replaceRuns(value, func(r rune) bool {
		return r == '\r' || r == '\n'
	})
}

func replaceRuns(value string, shouldReplace func(rune) bool) string {
	var builder strings.Builder
	previousWasReplacement := false
	for _, r := range value {
		if shouldReplace(r) {
			if !previousWasReplacement {
				builder.WriteByte(' ')
				previousWasReplacement = true
			}
			continue
		}

		builder.WriteRune(r)
		previousWasReplacement = false
	}
	return builder.String()
}

func riskForCommand(command string) string {
	lower := strings.ToLower(command)

	if strings.Contains(command, "git branch -D") {
		return "max"
	}

	if strings.Contains(lower, "rm -rf /") ||
		strings.Contains(lower, "rm -fr /") ||
		strings.Contains(lower, "mkfs") ||
		strings.Contains(lower, "dd if=") ||
		strings.Contains(lower, "diskutil erase") ||
		strings.Contains(lower, "git reset --hard") ||
		strings.Contains(lower, "git clean -fd") ||
		strings.Contains(lower, "git clean -df") ||
		strings.Contains(lower, "git push --force") ||
		strings.Contains(lower, "git push -f") ||
		strings.Contains(lower, "chmod -r 777 /") ||
		strings.Contains(lower, "chown -r ") ||
		strings.Contains(lower, "docker system prune") ||
		strings.Contains(lower, "docker volume prune") ||
		strings.Contains(lower, "drop database") {
		return "max"
	}

	if curlPipeShellRE.MatchString(lower) ||
		evalRE.MatchString(lower) ||
		sudoRE.MatchString(lower) {
		return "max"
	}

	if strings.Contains(lower, "git branch -d") ||
		strings.Contains(lower, "git branch --delete") ||
		strings.Contains(lower, "git rebase") ||
		strings.Contains(lower, "git push") ||
		strings.Contains(lower, "rm -rf") ||
		strings.Contains(lower, "rm -fr") ||
		(strings.Contains(lower, "find ") && strings.Contains(lower, "-delete")) ||
		(strings.Contains(lower, "find ") && strings.Contains(lower, "-exec rm")) ||
		strings.Contains(lower, "xargs rm") ||
		strings.Contains(lower, "kubectl delete") ||
		strings.Contains(lower, "docker rm") ||
		strings.Contains(lower, "docker rmi") ||
		strings.Contains(lower, "npm publish") ||
		strings.Contains(lower, "gh pr merge") ||
		strings.Contains(lower, "truncate table") {
		return "high"
	}

	if strings.Contains(lower, ">") ||
		strings.Contains(lower, "tee ") ||
		strings.Contains(lower, "sed -i") ||
		strings.Contains(lower, "perl -pi") ||
		strings.Contains(lower, "git commit") ||
		strings.Contains(lower, "git merge") ||
		strings.Contains(lower, "git checkout") ||
		strings.Contains(lower, "git switch") ||
		strings.Contains(lower, "git pull") ||
		strings.Contains(lower, "npm install") ||
		strings.Contains(lower, "npm add") ||
		strings.Contains(lower, "pnpm install") ||
		strings.Contains(lower, "yarn add") ||
		strings.Contains(lower, "pip install") ||
		strings.Contains(lower, "brew install") {
		return "med"
	}

	if containsWord(lower, "rm") ||
		containsWord(lower, "rmdir") ||
		containsWord(lower, "unlink") ||
		containsWord(lower, "mv") ||
		containsWord(lower, "cp") ||
		containsWord(lower, "mkdir") ||
		containsWord(lower, "touch") ||
		containsWord(lower, "chmod") {
		return "med"
	}

	if zeroRiskRE.MatchString(lower) || echoRiskRE.MatchString(lower) {
		return "zero"
	}

	if lowRiskRE.MatchString(lower) {
		return "low"
	}

	return "low"
}

func containsWord(haystack, needle string) bool {
	for start := 0; ; {
		index := strings.Index(haystack[start:], needle)
		if index == -1 {
			return false
		}

		index += start
		beforeOK := index == 0 || !isCommandWordByte(haystack[index-1])
		afterIndex := index + len(needle)
		afterOK := afterIndex == len(haystack) || !isCommandWordByte(haystack[afterIndex])
		if beforeOK && afterOK {
			return true
		}

		start = index + 1
	}
}

func isCommandWordByte(value byte) bool {
	r := rune(value)
	return unicode.IsLetter(r) || unicode.IsDigit(r) || value == '_' || value == '.' || value == '/' || value == '-'
}

func colorRisk(risk string, color bool) string {
	if !color {
		return risk
	}

	const (
		green   = "\033[32m"
		orange  = "\033[38;5;208m"
		red     = "\033[31m"
		boldRed = "\033[1;31m"
		reset   = "\033[0m"
	)

	switch risk {
	case "zero", "low":
		return green + risk + reset
	case "med":
		return orange + risk + reset
	case "high":
		return red + risk + reset
	case "max":
		return boldRed + risk + reset
	default:
		return risk
	}
}

func readChoice(in *os.File, out io.Writer, optionCount int) (string, error) {
	reader := bufio.NewReader(in)

	if isTerminal(in) {
		fmt.Fprintf(out, "\nRun option [1-%d], or q to cancel: ", optionCount)
		choice, ok, err := readSingleCharacter(in)
		if err != nil {
			return "", err
		}
		if ok {
			fmt.Fprintln(out)
			return strings.TrimSpace(choice), nil
		}

		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}

	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func readSingleCharacter(in *os.File) (string, bool, error) {
	stateCommand := exec.Command("stty", "-g")
	stateCommand.Stdin = in
	stateOutput, err := stateCommand.Output()
	if err != nil {
		return "", false, nil
	}

	previousState := strings.TrimSpace(string(stateOutput))
	rawCommand := exec.Command("stty", "-icanon", "min", "1", "time", "0")
	rawCommand.Stdin = in
	if err := rawCommand.Run(); err != nil {
		return "", false, nil
	}

	defer func() {
		restoreCommand := exec.Command("stty", previousState)
		restoreCommand.Stdin = in
		_ = restoreCommand.Run()
	}()

	var buffer [1]byte
	count, err := in.Read(buffer[:])
	if err != nil {
		return "", true, err
	}
	if count == 0 {
		return "", true, io.EOF
	}

	return string(buffer[0]), true, nil
}

func runBashCommand(command string) int {
	cmd := exec.Command("bash", "-c", command)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		die(err.Error())
		return 1
	}

	return 0
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

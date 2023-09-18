package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/alessio/shellescape"
	"github.com/fatih/color"
	"github.com/mattn/go-isatty"
	"github.com/pkg/term/termios"
	"golang.org/x/term"
)

var standardFdToFile = []*os.File{
	0: os.Stdin,
	1: os.Stdout,
	2: os.Stderr,
}

var noLongerSpawnChildren = atomic.Bool{}

var bold = color.New(color.Bold).SprintFunc()
var yellow = color.New(color.FgYellow).SprintFunc()

var stdoutIsTty = isatty.IsTerminal(uintptr(syscall.Stdout))

// stdoutAndStderrAreTheSame tells us if stdout and stderr point to the same file/pipe/stream, for the sole purpose
// of conserving pty/tty pairs - which are a very limited resource on most unix systems (linux default max: usually
// from 512 to 4096, macOS default max: from 127 to 512)
var stdoutAndStderrAreTheSame = func() bool {
	stdoutStat, err := os.Stdout.Stat()
	if err != nil {
		log.Fatalln("Cannot stat stdout:", err)
	}
	stdout, ok := stdoutStat.Sys().(*syscall.Stat_t)
	if !ok {
		// We probably aren't on a Unix - assume stdout and stderr are the same
		return false
	}

	stderrStat, err := os.Stderr.Stat()
	if err != nil {
		log.Fatalln("Cannot stat stderr:", err)
	}
	stderr, ok := stderrStat.Sys().(*syscall.Stat_t)
	if !ok {
		// We probably aren't on a Unix - assume stdout and stderr are the same
		return false
	}

	theyAre := stdout.Dev == stderr.Dev &&
		stdout.Ino == stderr.Ino &&
		stdout.Mode == stderr.Mode &&
		stdout.Nlink == stderr.Nlink &&
		stdout.Rdev == stderr.Rdev

	println("Are stdout and stderr the same:", theyAre)
	return theyAre
}()

func writeOut(out *Output) {
	var clearedOutBytes int64

	offset := 0
	for {
		fd, content, ok := out.getNextChunk(&offset)
		if !ok {
			break
		}

		_, _ = standardFdToFile[fd].Write(content)

		clearedOutBytes += chunkSizeWithHeader(content)
	}

	out.allocator.mustFree(out.parts)
	out.allocator.mustClose()
	out.parts = nil

	// Just deallocated a lot due to a child process dying, let's also hint Go to do the same
	debug.FreeOSMemory()

	mem.childDiedFreeingMemory.L.Lock()
	defer mem.childDiedFreeingMemory.L.Unlock()

	mem.currentlyStored.Add(-clearedOutBytes)
	mem.currentlyInTheForeground = out
	mem.childDiedFreeingMemory.Broadcast()
}

func toForeground(proc ProcessResult) (exitCode int) {
	proc.output.partsMutex.Lock()
	writeOut(proc.output)
	proc.output.shouldPassToParent = true
	proc.output.partsMutex.Unlock()

	err := proc.wait()

	// Check if our child exited unsuccessfully
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}

	if err != nil {
		log.Fatal(err)
	}
	return 0
}

func tryToIncreaseNoFile() {
	var rLimit syscall.Rlimit
	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit)
	if err != nil {
		return
	}
	rLimit.Cur = rLimit.Max
	_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit)
}

func waitForChildrenAfterAFailedOne(processes <-chan ProcessResult) {
	wg := sync.WaitGroup{}

	for processResult := range processes {
		processResult := processResult

		_ = processResult.cmd.Process.Signal(syscall.SIGTERM)

		wg.Add(1)
		go func() {
			_ = processResult.wait()
			wg.Done()
		}()
	}

	wg.Wait()
}

func instantiateCommandString(command []string, argument string) []string {
	if *flTemplate == "" {
		return append(command, argument)
	}

	replacedIn := 0

	for i, word := range command {
		if !strings.Contains(word, *flTemplate) {
			continue
		}

		command[i] = strings.ReplaceAll(command[i], *flTemplate, argument)
		replacedIn += 1
	}

	if replacedIn == 0 {
		// If there's no {}-template anywhere, let's just append the argument at the end
		return append(command, argument)
	} else {
		return command
	}
}

func resetTermStateBeforeExit(originalTermState *term.State) {
	if originalTermState != nil {
		err := term.Restore(syscall.Stdout, originalTermState)
		if err != nil {
			log.Printf("Warning: could not restore terminal state on exit: %v\n", err)
		}
	}
}

func startProcessesFromCliArguments(args Args, result chan<- ProcessResult) {
	for _, argument := range args.data {
		if noLongerSpawnChildren.Load() {
			break
		}

		processCommand := instantiateCommandString(slices.Clone(args.command), argument)
		result <- run(processCommand)
	}

	close(result)
}

func startProcessesFromStdin(args Args, result chan<- ProcessResult) {
	stdinReader := bufio.NewReader(os.Stdin)

	for {
		line, err := stdinReader.ReadString('\n')
		line = strings.TrimSuffix(line, "\n")

		if len(line) > 0 {
			processCommand := instantiateCommandString(slices.Clone(args.command), line)
			result <- run(processCommand)
		}

		if err == io.EOF {
			break
		} else if err != nil {
			log.Fatalf("Failed reading: %v\n", err)
		}
	}

	close(result)
}

func start(args Args) (exitCode int) {
	var originalTermState *term.State
	var err error

	if stdoutIsTty {
		originalTermState, err = term.GetState(syscall.Stdout)
		if err != nil {
			log.Printf("Warning: could get terminal state for stdout: %v\n", err)
		}
	}

	if originalTermState != nil {
		defer resetTermStateBeforeExit(originalTermState)

		signalledToExit := make(chan os.Signal, 1)
		signal.Notify(signalledToExit, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-signalledToExit
			resetTermStateBeforeExit(originalTermState)
			os.Exit(1)
		}()
	}

	// TODO: make flMaxProcesses be able to be 1
	processes := make(chan ProcessResult, *flMaxProcesses-2)
	go func() {
		if args.hasTripleColon {
			startProcessesFromCliArguments(args, processes)
		}
		if *flFromStdin {
			startProcessesFromStdin(args, processes)
		}
	}()

	firstProcess := true
	for processResult := range processes {
		if *flVerbose {
			quotedCommand := shellescape.QuoteCommand(processResult.cmd.Args)

			if firstProcess || !stdoutIsTty {
				_, _ = fmt.Fprintf(os.Stderr, bold("+ %s")+"\n", quotedCommand)
			} else if !processResult.isAlive() {
				_, _ = fmt.Fprintf(os.Stderr,
					bold("+ %s")+yellow(" (already finished, reporting saved output)")+"\n",
					quotedCommand)
			} else if -time.Until(processResult.startedAt) > 1*time.Second {
				_, _ = fmt.Fprintf(os.Stderr,
					bold("+ %s")+yellow(" (resumed output, already runnning for %v)")+"\n",
					quotedCommand,
					-time.Until(processResult.startedAt).Round(time.Second))
			} else {
				_, _ = fmt.Fprintf(os.Stderr, bold("+ %s")+"\n", quotedCommand)
			}
		}

		exitCode = max(exitCode, toForeground(processResult))

		if !*flKeepGoingOnError {
			if exitCode != 0 {
				noLongerSpawnChildren.Store(true)

				waitForChildrenAfterAFailedOne(processes)
				break
			}
		}

		firstProcess = false
	}

	return exitCode
}

func executeAndFlushTty(proc *exec.Cmd) (exitCode int) {
	if originalGomaxprocs := os.Getenv("_GPARALLEL_ORIGINAL_GOMAXPROCS"); originalGomaxprocs != "" {
		_ = os.Unsetenv("_GPARALLEL_ORIGINAL_GOMAXPROCS")
		_ = os.Setenv("GOMAXPROCS", originalGomaxprocs)
	} else {
		_ = os.Unsetenv("GOMAXPROCS")
	}

	proc.Stdin = os.Stdin
	proc.Stdout = os.Stdout
	proc.Stderr = os.Stderr
	err := proc.Start()
	if err != nil {
		log.Fatalf("Could not start process %v, %v\n", shellescape.QuoteCommand(proc.Args), err)
	}
	err = proc.Wait()

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode = exitErr.ExitCode()
	} else if err != nil {
		log.Fatalf("Could not wait for process %v, %v\n", shellescape.QuoteCommand(proc.Args), err)
	}

	_ = termios.Tcdrain(uintptr(syscall.Stdout))
	_ = termios.Tcdrain(uintptr(syscall.Stderr))

	return exitCode
}

func main() {
	log.SetFlags(0)
	log.SetPrefix(fmt.Sprintf("%s: ", os.Args[0]))

	args := parseArgs()

	if *flExecuteAndFlushTty {
		os.Exit(executeAndFlushTty(exec.Command(args.command[0], args.command[1:]...)))
	}

	tryToIncreaseNoFile()

	exitCode := start(args)

	os.Exit(exitCode)
}

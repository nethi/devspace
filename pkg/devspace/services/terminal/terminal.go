package terminal

import (
	"fmt"
	"github.com/loft-sh/devspace/pkg/devspace/config/versions/latest"
	devspacecontext "github.com/loft-sh/devspace/pkg/devspace/context"
	"github.com/loft-sh/devspace/pkg/devspace/kubectl"
	"github.com/loft-sh/devspace/pkg/devspace/services/targetselector"
	interruptpkg "github.com/loft-sh/devspace/pkg/util/interrupt"
	"github.com/loft-sh/devspace/pkg/util/log"
	"github.com/loft-sh/devspace/pkg/util/tomb"
	"github.com/mgutz/ansi"
	"github.com/sirupsen/logrus"
	"io"
	kubectlExec "k8s.io/client-go/util/exec"
	"k8s.io/kubectl/pkg/util/term"
	"time"
)

// StartTerminalFromCMD opens a new terminal
func StartTerminalFromCMD(
	ctx *devspacecontext.Context,
	selector targetselector.TargetSelector,
	command []string,
	wait,
	restart bool,
	stdout io.Writer,
	stderr io.Writer,
	stdin io.Reader,
) (int, error) {
	container, err := selector.SelectSingleContainer(ctx.Context, ctx.KubeClient, ctx.Log)
	if err != nil {
		return 0, err
	}

	ctx.Log.Infof("Opening shell to pod:container %s:%s", ansi.Color(container.Pod.Name, "white+b"), ansi.Color(container.Container.Name, "white+b"))
	done := make(chan error)
	go func() {
		interruptpkg.Global.Stop()
		defer interruptpkg.Global.Start()

		done <- ctx.KubeClient.ExecStream(ctx.Context, &kubectl.ExecStreamOptions{
			Pod:         container.Pod,
			Container:   container.Container.Name,
			Command:     command,
			TTY:         true,
			Stdin:       stdin,
			Stdout:      stdout,
			Stderr:      stderr,
			SubResource: kubectl.SubResourceExec,
		})
	}()

	// wait until either client has finished or we got interrupted
	select {
	case <-ctx.Context.Done():
		<-done
		return 0, nil
	case err = <-done:
		if err != nil {
			if exitError, ok := err.(kubectlExec.CodeExitError); ok {
				// Expected exit codes are (https://shapeshed.com/unix-exit-codes/):
				// 1 - Catchall for general errors
				// 2 - Misuse of shell builtins (according to Bash documentation)
				// 126 - Command invoked cannot execute
				// 127 - “command not found”
				// 128 - Invalid argument to exit
				// 130 - Script terminated by Control-C
				if restart && IsUnexpectedExitCode(exitError.Code) {
					ctx.Log.WriteString(logrus.InfoLevel, "\n")
					ctx.Log.Infof("Restarting terminal because: %s", err)
					return StartTerminalFromCMD(ctx, selector, command, wait, restart, stdout, stderr, stdin)
				}

				return exitError.Code, nil
			} else if restart {
				ctx.Log.WriteString(logrus.InfoLevel, "\n")
				ctx.Log.Infof("Restarting terminal because: %s", err)
				return StartTerminalFromCMD(ctx, selector, command, wait, restart, stdout, stderr, stdin)
			}

			return 0, err
		}
	}

	return 0, nil
}

// StartTerminal opens a new terminal
func StartTerminal(
	ctx *devspacecontext.Context,
	devContainer *latest.DevContainer,
	selector targetselector.TargetSelector,
	stdout io.Writer,
	stderr io.Writer,
	stdin io.Reader,
	parent *tomb.Tomb,
) (err error) {
	// restart on error
	defer func() {
		if err != nil {
			if ctx.IsDone() {
				return
			}

			ctx.Log.WriteString(logrus.InfoLevel, "\n")
			ctx.Log.Infof("Restarting terminal because: %s", err)
			time.Sleep(time.Second * 3)
			err = StartTerminal(ctx, devContainer, selector, stdout, stderr, stdin, parent)
			return
		}
	}()

	before := log.GetBaseInstance().GetLevel()
	log.GetBaseInstance().SetLevel(logrus.PanicLevel)
	defer log.GetBaseInstance().SetLevel(before)

	command := getCommand(devContainer)
	container, err := selector.WithContainer(devContainer.Container).SelectSingleContainer(ctx.Context, ctx.KubeClient, ctx.Log)
	if err != nil {
		return err
	}

	ctx.Log.Infof("Opening shell to pod:container %s:%s", ansi.Color(container.Pod.Name, "white+b"), ansi.Color(container.Container.Name, "white+b"))
	errChan := make(chan error)
	parent.Go(func() error {
		interruptpkg.Global.Stop()
		defer interruptpkg.Global.Start()

		// try to install screen
		useScreen := false
		if term.IsTerminal(stdin) && !devContainer.Terminal.DisableScreen {
			bufferStdout, bufferStderr, err := ctx.KubeClient.ExecBuffered(ctx.Context, container.Pod, container.Container.Name, []string{
				"sh",
				"-c",
				`if ! command -v screen; then
  if command -v apk; then
    apk add --no-cache screen
  elif command -v apt-get; then
    apt-get -qq update && apt-get install -y screen && rm -rf /var/lib/apt/lists/*
  else
    echo "Couldn't install screen using neither apt-get nor apk."
    exit 1
  fi
fi
if command -v screen; then
  echo "Screen installed successfully."

  if [ ! -f ~/.screenrc ]; then
    echo "termcapinfo xterm* ti@:te@" > ~/.screenrc
  fi
else
  echo "Couldn't find screen, need to fallback."
  exit 1
fi`,
			}, nil)
			if err != nil {
				ctx.Log.Debugf("Error installing screen: %s %s %v", string(bufferStdout), string(bufferStderr), err)
			} else {
				useScreen = true
			}
		}
		if useScreen {
			newCommand := []string{"screen", "-dRSqL", "dev"}
			newCommand = append(newCommand, command...)
			command = newCommand
		}

		errChan <- ctx.KubeClient.ExecStream(ctx.Context, &kubectl.ExecStreamOptions{
			Pod:         container.Pod,
			Container:   container.Container.Name,
			Command:     command,
			TTY:         true,
			Stdin:       stdin,
			Stdout:      stdout,
			Stderr:      stderr,
			SubResource: kubectl.SubResourceExec,
		})
		return nil
	})

	select {
	case <-ctx.Context.Done():
		<-errChan
		return nil
	case err = <-errChan:
		if ctx.IsDone() {
			return nil
		}

		if err != nil {
			// check if context is done
			if exitError, ok := err.(kubectlExec.CodeExitError); ok {
				// Expected exit codes are (https://shapeshed.com/unix-exit-codes/):
				// 1 - Catchall for general errors
				// 2 - Misuse of shell builtins (according to Bash documentation)
				// 126 - Command invoked cannot execute
				// 127 - “command not found”
				// 128 - Invalid argument to exit
				// 130 - Script terminated by Control-C
				if IsUnexpectedExitCode(exitError.Code) {
					return err
				}

				return nil
			}

			return fmt.Errorf("lost connection to pod %s: %v", container.Pod.Name, err)
		}
	}

	return nil
}

func IsUnexpectedExitCode(code int) bool {
	// Expected exit codes are (https://shapeshed.com/unix-exit-codes/):
	// 1 - Catchall for general errors
	// 2 - Misuse of shell builtins (according to Bash documentation)
	// 126 - Command invoked cannot execute
	// 127 - “command not found”
	// 128 - Invalid argument to exit
	// 130 - Script terminated by Control-C
	return code != 0 && code != 1 && code != 2 && code != 126 && code != 127 && code != 128 && code != 130
}

func getCommand(devContainer *latest.DevContainer) []string {
	command := devContainer.Terminal.Command
	if command == "" {
		command = "command -v bash >/dev/null 2>&1 && exec bash || exec sh"
	}

	if devContainer.Terminal.WorkDir != "" {
		return []string{"sh", "-c", fmt.Sprintf("cd %s; %s", devContainer.Terminal.WorkDir, command)}
	}

	return []string{"sh", "-c", fmt.Sprintf("%s", command)}
}

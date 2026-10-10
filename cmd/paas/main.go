// Command paas is the local CLI client. Commands stay thin and talk to paasd
// through the Unix-socket HTTP protocol.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"paas/internal/daemon"
	"paas/internal/model"
)

const usage = "usage: paas [-socket PATH] {ping|deploy SOURCE [--name NAME]|apps|inspect NAME|logs NAME|stop NAME}"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the CLI and returns the process status code. It does not exit,
// so callers can exercise the command behavior directly.
func run(args []string, stdout, stderr io.Writer) int {
	socket, command, commandArgs, err := parseInvocation(args)
	if err != nil {
		return reportUsageError(stderr, err)
	}

	switch command {
	case "ping", "deploy", "apps", "inspect", "logs", "stop":
	default:
		return reportUsageError(stderr, fmt.Errorf("unknown command %q", command))
	}

	request, err := parseCommandRequest(command, commandArgs)
	if err != nil {
		return reportUsageError(stderr, err)
	}

	timeout := 5 * time.Second
	if command == "deploy" {
		timeout = 6 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if command == "ping" {
		result, err := daemon.Ping(ctx, socket)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if _, err := fmt.Fprintln(stdout, result); err != nil {
			fmt.Fprintf(stderr, "write ping response: %v\n", err)
			return 1
		}
		return 0
	}

	response, executeErr := daemon.Execute(ctx, socket, command, request)
	if command == "deploy" {
		return reportDeployment(stdout, stderr, request, response, executeErr)
	}
	if err := responseError(response, executeErr); err != nil {
		fmt.Fprintf(stderr, "paas %s: %v\n", command, err)
		return 1
	}

	switch command {
	case "apps":
		if err := printApps(stdout, response.Apps); err != nil {
			fmt.Fprintf(stderr, "write apps: %v\n", err)
			return 1
		}
	case "inspect":
		if err := printInspection(stdout, request.Name, response); err != nil {
			fmt.Fprintf(stderr, "write inspection: %v\n", err)
			return 1
		}
	case "logs":
		if _, err := io.WriteString(stdout, response.Logs); err != nil {
			fmt.Fprintf(stderr, "write logs: %v\n", err)
			return 1
		}
	case "stop":
		if _, err := fmt.Fprintf(stdout, "Stopped %s\n", request.Name); err != nil {
			fmt.Fprintf(stderr, "write stop confirmation: %v\n", err)
			return 1
		}
	}
	return 0
}

func reportUsageError(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, err)
	fmt.Fprintln(stderr, usage)
	return 2
}

func responseError(response *daemon.Response, err error) error {
	if err != nil {
		return err
	}
	if response == nil {
		return fmt.Errorf("daemon returned an empty response")
	}
	if response.Error != "" {
		return fmt.Errorf("%s", response.Error)
	}
	return nil
}

func reportDeployment(stdout, stderr io.Writer, request daemon.Request, response *daemon.Response, executeErr error) int {
	deployment := deploymentFromResponse(response, request.Name)
	responseMessage := ""
	if response != nil {
		responseMessage = response.Error
	}

	if executeErr != nil || responseMessage != "" || deployment == nil || deployment.State != model.Running {
		if deployment != nil {
			state := string(deployment.State)
			if state == "" {
				state = "UNKNOWN"
			}
			fmt.Fprintf(stdout, "State: %s\nDeployment: %s\n", state, displayValue(deployment.ID))
		}

		message := responseMessage
		if message == "" && deployment != nil {
			message = deployment.Error
		}
		if message == "" && executeErr != nil {
			message = executeErr.Error()
		}
		if message == "" {
			message = "deployment did not reach RUNNING"
		}
		fmt.Fprintf(stderr, "paas deploy: %s\n", message)
		return 1
	}

	name := deployment.AppName
	if name == "" && response != nil && len(response.Apps) > 0 {
		name = response.Apps[0].Application.Name
	}
	if name == "" {
		name = request.Name
	}
	url := ""
	if response != nil {
		url = response.URL
	}
	if url == "" {
		url = fmt.Sprintf("http://%s.localhost:8080", name)
	}
	if _, err := fmt.Fprintf(stdout, "State: %s\nRelease: %s\nPort: %d\nURL: %s\n",
		deployment.State, displayValue(deployment.ReleaseID), deployment.Port, url); err != nil {
		fmt.Fprintf(stderr, "write deployment result: %v\n", err)
		return 1
	}
	return 0
}

func deploymentFromResponse(response *daemon.Response, name string) *model.Deployment {
	if response == nil {
		return nil
	}
	if response.Deployment != nil {
		return response.Deployment
	}
	for _, app := range response.Apps {
		if app.Deployment == nil {
			continue
		}
		if name == "" || app.Application.Name == name || app.Deployment.AppName == name {
			return app.Deployment
		}
	}
	if len(response.Apps) == 1 {
		return response.Apps[0].Deployment
	}
	return nil
}

func printApps(stdout io.Writer, apps []daemon.AppView) error {
	if _, err := fmt.Fprintln(stdout, "NAME STATE RELEASE PORT"); err != nil {
		return err
	}
	for _, app := range apps {
		name := app.Application.Name
		if name == "" && app.Deployment != nil {
			name = app.Deployment.AppName
		}
		if name == "" {
			name = "-"
		}
		state, release, port := "-", "-", "-"
		if app.Deployment != nil {
			state = stateValue(app.Deployment.State)
			release = displayValue(app.Deployment.ReleaseID)
			port = strconv.Itoa(app.Deployment.Port)
		}
		if _, err := fmt.Fprintf(stdout, "%s %s %s %s\n", name, state, release, port); err != nil {
			return err
		}
	}
	return nil
}

func printInspection(stdout io.Writer, requestedName string, response *daemon.Response) error {
	var appName string
	var deployment *model.Deployment
	for _, app := range response.Apps {
		if app.Application.Name == requestedName || app.Deployment != nil && app.Deployment.AppName == requestedName {
			appName = app.Application.Name
			deployment = app.Deployment
			break
		}
	}
	if appName == "" && len(response.Apps) == 1 {
		appName = response.Apps[0].Application.Name
		deployment = response.Apps[0].Deployment
	}
	if deployment == nil {
		deployment = response.Deployment
	}
	if appName == "" && deployment != nil {
		appName = deployment.AppName
	}
	if appName == "" {
		appName = requestedName
	}

	release, deploymentID, state, pid, port, lastError := "-", "-", "-", "-", "-", "-"
	if deployment != nil {
		release = displayValue(deployment.ReleaseID)
		deploymentID = displayValue(deployment.ID)
		state = stateValue(deployment.State)
		pid = strconv.Itoa(deployment.PID)
		port = strconv.Itoa(deployment.Port)
		lastError = displayValue(deployment.Error)
	}
	_, err := fmt.Fprintf(stdout, "Application: %s\nRelease: %s\nDeployment: %s\nState: %s\nPID: %s\nPort: %s\nLast error: %s\n",
		displayValue(appName), release, deploymentID, state, pid, port, lastError)
	if err != nil {
		return err
	}
	if deployment != nil && deployment.SandboxType != "" {
		isolation := "no"
		if deployment.SandboxType == "systemd" || deployment.SandboxType == "bubblewrap" {
			isolation = "yes"
		}
		if _, err := fmt.Fprintf(stdout, "\nSandbox:\n  Backend: %s\n  PID isolation:     %s\n  Mount isolation:   %s\n  IPC isolation:     %s\n  UTS isolation:     %s\n  User isolation:    %s\n  Network isolation: no\n", deployment.SandboxType, isolation, isolation, isolation, isolation, isolation); err != nil {
			return err
		}
		if deployment.SandboxType == "systemd" {
			if _, err := fmt.Fprintf(stdout, "\nResources:\n  Memory limit: %s\n  CPU quota:    %s%%\n  Tasks limit:  %d\n", deployment.MemoryLimit, strconv.FormatFloat(deployment.CPUQuota*100, 'f', -1, 64), deployment.TasksLimit); err != nil {
				return err
			}
			if deployment.SystemdUnit != "" {
				if _, err := fmt.Fprintf(stdout, "\nSystemd:\n  Unit: %s\n", deployment.SystemdUnit); err != nil {
					return err
				}
			}
		} else {
			if _, err := fmt.Fprint(stdout, "\nResources:\n  Limits: not enforced by this backend\n"); err != nil {
				return err
			}
		}
	}
	if deployment != nil && deployment.State == model.OOMKilled {
		if _, err := fmt.Fprintf(stdout, "\nFailure:\n  Result: oom-kill\n  Cause:  Memory limit exceeded (%s)\n", displayValue(deployment.MemoryLimit)); err != nil {
			return err
		}
	}
	return nil
}

func stateValue(state model.State) string {
	if state == "" {
		return "-"
	}
	return string(state)
}

func displayValue(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func parseCommandRequest(command string, args []string) (daemon.Request, error) {
	var request daemon.Request
	switch command {
	case "ping", "apps":
		if len(args) > 0 {
			return request, commandArgumentError(command, args[0])
		}
	case "deploy":
		var source string
		nameSet := false
		for index := 0; index < len(args); index++ {
			arg := args[index]
			switch {
			case arg == "--name":
				if nameSet {
					return request, fmt.Errorf("--name may only be specified once")
				}
				if index+1 >= len(args) || args[index+1] == "" || strings.HasPrefix(args[index+1], "-") {
					return request, fmt.Errorf("--name requires a name")
				}
				request.Name = args[index+1]
				nameSet = true
				index++
			case strings.HasPrefix(arg, "-"):
				return request, fmt.Errorf("unknown option %q", arg)
			case source == "":
				source = arg
			default:
				return request, fmt.Errorf("deploy accepts one SOURCE")
			}
		}
		if source == "" {
			return request, fmt.Errorf("deploy requires SOURCE")
		}
		absoluteSource, err := filepath.Abs(source)
		if err != nil {
			return request, fmt.Errorf("resolve deploy source %q: %w", source, err)
		}
		info, err := os.Stat(absoluteSource)
		if err != nil {
			return request, fmt.Errorf("inspect deploy source %q: %w", absoluteSource, err)
		}
		if !info.IsDir() {
			return request, fmt.Errorf("deploy source %q is not a directory", absoluteSource)
		}
		request.SourceDir = absoluteSource
	case "inspect", "logs", "stop":
		if len(args) == 0 || args[0] == "" {
			return request, fmt.Errorf("%s requires NAME", command)
		}
		if strings.HasPrefix(args[0], "-") {
			return request, fmt.Errorf("unknown option %q", args[0])
		}
		if len(args) > 1 {
			return request, commandArgumentError(command, args[1])
		}
		request.Name = args[0]
	}
	return request, nil
}

func commandArgumentError(command, argument string) error {
	if strings.HasPrefix(argument, "-") {
		return fmt.Errorf("unknown option %q", argument)
	}
	return fmt.Errorf("%s does not accept argument %q", command, argument)
}

func parseArgs(args []string) (socket string, command string, err error) {
	socket, command, commandArgs, err := parseInvocation(args)
	if err != nil {
		return "", "", err
	}
	if len(commandArgs) > 0 {
		return "", "", fmt.Errorf("unexpected argument %q", commandArgs[0])
	}
	return socket, command, nil
}

func parseInvocation(args []string) (socket string, command string, commandArgs []string, err error) {
	socket = os.Getenv("PAASD_SOCKET")
	if socket == "" {
		socket = daemon.DefaultSocketPath
	}

	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-socket" || arg == "--socket":
			if index+1 >= len(args) || args[index+1] == "" || strings.HasPrefix(args[index+1], "-") {
				return "", "", nil, fmt.Errorf("%s requires a path", arg)
			}
			socket = args[index+1]
			index++
		case strings.HasPrefix(arg, "-socket="):
			socket = strings.TrimPrefix(arg, "-socket=")
			if socket == "" {
				return "", "", nil, fmt.Errorf("-socket requires a path")
			}
		case strings.HasPrefix(arg, "--socket="):
			socket = strings.TrimPrefix(arg, "--socket=")
			if socket == "" {
				return "", "", nil, fmt.Errorf("--socket requires a path")
			}
		case command == "" && strings.HasPrefix(arg, "-"):
			return "", "", nil, fmt.Errorf("unknown option %q", arg)
		case command == "":
			command = arg
		default:
			commandArgs = append(commandArgs, arg)
		}
	}

	if command == "" {
		return "", "", nil, fmt.Errorf("expected exactly one command")
	}
	return socket, command, commandArgs, nil
}

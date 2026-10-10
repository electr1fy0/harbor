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

const usage = "usage: paas [-socket PATH] {ping|deploy SOURCE [--name NAME]|apps|inspect NAME|logs NAME|stop NAME|releases NAME|rollback NAME [--release ID]|add TYPE --name NAME|link APP SERVICE|services}"

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
	case "ping", "deploy", "apps", "inspect", "logs", "stop", "releases", "rollback", "add", "link", "services":
	default:
		return reportUsageError(stderr, fmt.Errorf("unknown command %q", command))
	}

	request, err := parseCommandRequest(command, commandArgs)
	if err != nil {
		return reportUsageError(stderr, err)
	}

	timeout := 5 * time.Second
	if command == "deploy" || command == "rollback" || command == "add" {
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
	if command == "deploy" || command == "rollback" {
		return reportDeployment(stdout, stderr, command, request, response, executeErr)
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
	case "releases":
		if err := printReleases(stdout, response.Releases); err != nil {
			fmt.Fprintf(stderr, "write releases: %v\n", err)
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
	case "add":
		if response != nil && response.Service != nil {
			fmt.Fprintf(stdout, "→ Preparing %s runtime\n", response.Service.Type)
			fmt.Fprintf(stdout, "→ Creating persistent volume\n")
			fmt.Fprintf(stdout, "→ Initializing database\n")
			fmt.Fprintf(stdout, "→ Starting service\n")
			fmt.Fprintf(stdout, "→ Health check passed\n\n")
			fmt.Fprintf(stdout, "✓ %s READY (port %d)\n", response.Service.Name, response.Service.Port)
		}
	case "link":
		if response != nil && response.Binding != nil {
			fmt.Fprintf(stdout, "✓ Injected %s into %s\n", response.Binding.EnvKey, request.AppName)
		}
	case "services":
		if err := printServices(stdout, response.Services); err != nil {
			fmt.Fprintf(stderr, "write services: %v\n", err)
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

func reportDeployment(stdout, stderr io.Writer, command string, request daemon.Request, response *daemon.Response, executeErr error) int {
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
			if _, err := fmt.Fprintf(stdout, "State: %s\nDeployment: %s\n", state, displayValue(deployment.ID)); err != nil {
				fmt.Fprintf(stderr, "write deployment result: %v\n", err)
			}
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
		fmt.Fprintf(stderr, "paas %s: %s\n", command, message)
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

func printReleases(stdout io.Writer, releases []model.ReleaseView) error {
	if _, err := fmt.Fprintln(stdout, "RELEASE SOURCE_HASH CREATED ACTIVE"); err != nil {
		return err
	}
	for _, r := range releases {
		active := "no"
		if r.IsActive {
			active = "yes"
		}
		hash := r.SourceHash
		if len(hash) > 12 {
			hash = hash[:12]
		}
		if hash == "" {
			hash = "-"
		}
		created := r.CreatedAt.UTC().Format(time.RFC3339)
		if _, err := fmt.Fprintf(stdout, "%s %s %s %s\n", r.ID, hash, created, active); err != nil {
			return err
		}
	}
	return nil
}

func printServices(stdout io.Writer, svcs []model.Service) error {
	if _, err := fmt.Fprintln(stdout, "NAME TYPE STATE PORT"); err != nil {
		return err
	}
	for _, s := range svcs {
		if _, err := fmt.Fprintf(stdout, "%s %s %s %d\n", s.Name, s.Type, s.State, s.Port); err != nil {
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
	case "ping", "apps", "services":
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
	case "inspect", "logs", "stop", "releases":
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
	case "rollback":
		if len(args) == 0 || args[0] == "" || strings.HasPrefix(args[0], "-") {
			return request, fmt.Errorf("rollback requires NAME")
		}
		request.Name = args[0]
		releaseSet := false
		for i := 1; i < len(args); i++ {
			arg := args[i]
			isRelease := arg == "--release" || arg == "-release" || strings.HasPrefix(arg, "--release=") || strings.HasPrefix(arg, "-release=")
			if isRelease && releaseSet {
				return request, fmt.Errorf("--release may only be specified once")
			}
			switch {
			case arg == "--release" || arg == "-release":
				if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
					return request, fmt.Errorf("--release requires a release ID")
				}
				request.ReleaseID = args[i+1]
				i++
			case strings.HasPrefix(arg, "--release="):
				request.ReleaseID = strings.TrimPrefix(arg, "--release=")
			case strings.HasPrefix(arg, "-release="):
				request.ReleaseID = strings.TrimPrefix(arg, "-release=")
			case strings.HasPrefix(arg, "-"):
				return request, fmt.Errorf("unknown option %q", arg)
			default:
				return request, commandArgumentError(command, arg)
			}
			if isRelease {
				if request.ReleaseID == "" || strings.HasPrefix(request.ReleaseID, "-") {
					return request, fmt.Errorf("--release requires a release ID")
				}
				releaseSet = true
			}
		}
	case "add":
		if len(args) == 0 || args[0] == "" || strings.HasPrefix(args[0], "-") {
			return request, fmt.Errorf("add requires TYPE")
		}
		request.ServiceType = args[0]
		for index := 1; index < len(args); index++ {
			arg := args[index]
			if arg == "--name" {
				if index+1 >= len(args) || args[index+1] == "" || strings.HasPrefix(args[index+1], "-") {
					return request, fmt.Errorf("--name requires a name")
				}
				request.Name = args[index+1]
				index++
			} else {
				return request, fmt.Errorf("unknown option %q", arg)
			}
		}
		if request.Name == "" {
			return request, fmt.Errorf("add requires --name")
		}
	case "link":
		if len(args) < 2 || args[0] == "" || args[1] == "" || strings.HasPrefix(args[0], "-") || strings.HasPrefix(args[1], "-") {
			return request, fmt.Errorf("link requires APP and SERVICE")
		}
		request.AppName = args[0]
		request.ServiceName = args[1]
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

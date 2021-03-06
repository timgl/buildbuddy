package event_parser

import (
	"bytes"
	"regexp"
	"strings"
	"time"

	"github.com/buildbuddy-io/buildbuddy/proto/build_event_stream"
	"github.com/buildbuddy-io/buildbuddy/proto/command_line"
	"github.com/buildbuddy-io/buildbuddy/server/terminal"

	inpb "github.com/buildbuddy-io/buildbuddy/proto/invocation"
)

const (
	envVarPrefix              = "--"
	envVarOptionName          = "client_env"
	envVarSeparator           = "="
	envVarRedactedPlaceholder = "<REDACTED>"
	urlSecretRegexString      = `\:\/\/.*\@`
)

func filterCommandLine(in *command_line.CommandLine) *command_line.CommandLine {
	if in == nil {
		return nil
	}
	urlSecretRegex := regexp.MustCompile(urlSecretRegexString)
	var out command_line.CommandLine
	out = *in
	for _, section := range out.Sections {
		switch p := section.SectionType.(type) {
		case *command_line.CommandLineSection_OptionList:
			{
				for _, option := range p.OptionList.Option {
					if strings.Contains(option.OptionValue, "@") {
						option.OptionValue = urlSecretRegex.ReplaceAllString(option.OptionValue, "://"+envVarRedactedPlaceholder+"@")
						option.CombinedForm = urlSecretRegex.ReplaceAllString(option.CombinedForm, "://"+envVarRedactedPlaceholder+"@")
					}
					if option.OptionName == envVarOptionName {
						parts := strings.Split(option.OptionValue, envVarSeparator)
						option.OptionValue = strings.Join([]string{parts[0], envVarRedactedPlaceholder}, envVarSeparator)
						option.CombinedForm = envVarPrefix + envVarOptionName + envVarSeparator + parts[0] + envVarSeparator + envVarRedactedPlaceholder
					}
				}
			}
		default:
			continue
		}
	}
	return &out
}

func filterUnstructuredCommandLine(in *build_event_stream.UnstructuredCommandLine) *build_event_stream.UnstructuredCommandLine {
	if in == nil {
		return nil
	}
	urlSecretRegex := regexp.MustCompile(urlSecretRegexString)
	var out build_event_stream.UnstructuredCommandLine
	out = *in
	for i, arg := range out.Args {
		if strings.Contains(arg, "@") {
			out.Args[i] = urlSecretRegex.ReplaceAllString(arg, "://"+envVarRedactedPlaceholder+"@")
		}
		if strings.HasPrefix(arg, envVarPrefix+envVarOptionName) {
			parts := strings.Split(arg, envVarSeparator)
			if len(parts) < 2 {
				continue
			}
			out.Args[i] = envVarPrefix + envVarOptionName + envVarSeparator + parts[1] + envVarSeparator + envVarRedactedPlaceholder
		}
	}
	return &out
}

func FillInvocationFromEvents(buildEvents []*inpb.InvocationEvent, invocation *inpb.Invocation) {
	startTimeMillis := int64(-1)
	endTimeMillis := int64(-1)

	var consoleBuffer bytes.Buffer

	for _, event := range buildEvents {
		invocation.Event = append(invocation.Event, event)

		switch p := event.BuildEvent.Payload.(type) {
		case *build_event_stream.BuildEvent_Progress:
			{
				consoleBuffer.Write([]byte(p.Progress.Stderr))
				consoleBuffer.Write([]byte(p.Progress.Stdout))
				// Clear progress event values as we've got them via ConsoleBuffer and they take up a lot of space.
				p.Progress.Stderr = ""
				p.Progress.Stdout = ""
			}
		case *build_event_stream.BuildEvent_Aborted:
			{
			}
		case *build_event_stream.BuildEvent_Started:
			{
				startTimeMillis = p.Started.StartTimeMillis
				invocation.Command = p.Started.Command
				for _, child := range event.BuildEvent.Children {
					// Here we are then. Knee-deep.
					switch c := child.Id.(type) {
					case *build_event_stream.BuildEventId_Pattern:
						{
							invocation.Pattern = c.Pattern.Pattern
						}
					}
				}
			}
		case *build_event_stream.BuildEvent_UnstructuredCommandLine:
			{
				p.UnstructuredCommandLine = filterUnstructuredCommandLine(p.UnstructuredCommandLine)
			}
		case *build_event_stream.BuildEvent_StructuredCommandLine:
			{
				filteredCL := filterCommandLine(p.StructuredCommandLine)
				if filteredCL != nil {
					invocation.StructuredCommandLine = append(invocation.StructuredCommandLine, filteredCL)
				}
			}
		case *build_event_stream.BuildEvent_OptionsParsed:
			{
			}
		case *build_event_stream.BuildEvent_WorkspaceStatus:
			{
				for _, item := range p.WorkspaceStatus.Item {
					switch item.Key {
					case "BUILD_USER":
						invocation.User = item.Value
					case "BUILD_HOST":
						invocation.Host = item.Value
					}
				}
			}
		case *build_event_stream.BuildEvent_Fetch:
			{
			}
		case *build_event_stream.BuildEvent_Configuration:
			{
			}
		case *build_event_stream.BuildEvent_Expanded:
			{
			}
		case *build_event_stream.BuildEvent_Configured:
			{
			}
		case *build_event_stream.BuildEvent_Action:
			{
			}
		case *build_event_stream.BuildEvent_NamedSetOfFiles:
			{
			}
		case *build_event_stream.BuildEvent_Completed:
			{
			}
		case *build_event_stream.BuildEvent_TestResult:
			{
			}
		case *build_event_stream.BuildEvent_TestSummary:
			{
			}
		case *build_event_stream.BuildEvent_Finished:
			{
				endTimeMillis = p.Finished.FinishTimeMillis
				invocation.Success = p.Finished.ExitCode.Code == 0
			}
		case *build_event_stream.BuildEvent_BuildToolLogs:
			{
			}
		case *build_event_stream.BuildEvent_BuildMetrics:
			{
				invocation.ActionCount = p.BuildMetrics.ActionSummary.ActionsExecuted
			}
		case *build_event_stream.BuildEvent_WorkspaceInfo:
			{
			}
		case *build_event_stream.BuildEvent_BuildMetadata:
			{
			}
		case *build_event_stream.BuildEvent_ConvenienceSymlinksIdentified:
			{
			}
		}
	}

	buildDuration := time.Duration((endTimeMillis - startTimeMillis) * int64(time.Millisecond))
	invocation.DurationUsec = buildDuration.Microseconds()
	// TODO(siggisim): Do this rendering once on write, rather than on every read.
	invocation.ConsoleBuffer = string(terminal.RenderAsANSI(consoleBuffer.Bytes()))
}

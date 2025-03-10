package pods

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containers/common/pkg/completion"
	"github.com/containers/common/pkg/report"
	"github.com/containers/podman/v4/cmd/podman/common"
	"github.com/containers/podman/v4/cmd/podman/registry"
	"github.com/containers/podman/v4/cmd/podman/utils"
	"github.com/containers/podman/v4/pkg/domain/entities"
	systemDefine "github.com/containers/podman/v4/pkg/systemd/define"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

const (
	startTimeoutFlagName      = "start-timeout"
	stopTimeoutFlagName       = "stop-timeout"
	stopTimeoutCompatFlagName = "time"
	restartPolicyFlagName     = "restart-policy"
	restartSecFlagName        = "restart-sec"
	newFlagName               = "new"
)

var (
	files              bool
	format             string
	systemdRestart     string
	systemdRestartSec  uint
	startTimeout       uint
	stopTimeout        uint
	systemdOptions     = entities.GenerateSystemdOptions{}
	systemdDescription = `Generate systemd units for a pod or container.
  The generated units can later be controlled via systemctl(1).`

	systemdCmd = &cobra.Command{
		Use:               "systemd [options] {CONTAINER|POD}",
		Short:             "Generate systemd units.",
		Long:              systemdDescription,
		RunE:              systemd,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: common.AutocompleteContainersAndPods,
		Example: `podman generate systemd CTR
  podman generate systemd --new --time 10 CTR
  podman generate systemd --files --name POD`,
	}
)

func init() {
	registry.Commands = append(registry.Commands, registry.CliCommand{
		Command: systemdCmd,
		Parent:  generateCmd,
	})
	flags := systemdCmd.Flags()
	flags.BoolVarP(&systemdOptions.Name, "name", "n", false, "Use container/pod names instead of IDs")
	flags.BoolVarP(&files, "files", "f", false, "Generate .service files instead of printing to stdout")
	flags.BoolVar(&systemdOptions.TemplateUnitFile, "template", false, "Make it a template file and use %i and %I specifiers. Working only for containers")

	flags.UintVarP(&startTimeout, startTimeoutFlagName, "", 0, "Start timeout override")
	_ = systemdCmd.RegisterFlagCompletionFunc(startTimeoutFlagName, completion.AutocompleteNone)

	// NOTE: initially, there was only a --time/-t flag which mapped to
	// stop-timeout. To remain backwards compatible create a hidden flag
	// that maps to StopTimeout.
	flags.UintVarP(&stopTimeout, stopTimeoutFlagName, "", containerConfig.Engine.StopTimeout, "Stop timeout override")
	_ = systemdCmd.RegisterFlagCompletionFunc(stopTimeoutFlagName, completion.AutocompleteNone)
	flags.UintVarP(&stopTimeout, stopTimeoutCompatFlagName, "t", containerConfig.Engine.StopTimeout, "Backwards alias for --stop-timeout")
	_ = flags.MarkHidden("time")

	flags.BoolVar(&systemdOptions.New, newFlagName, false, "Create a new container or pod instead of starting an existing one")
	flags.BoolVarP(&systemdOptions.NoHeader, "no-header", "", false, "Skip header generation")

	containerPrefixFlagName := "container-prefix"
	flags.StringVar(&systemdOptions.ContainerPrefix, containerPrefixFlagName, "container", "Systemd unit name prefix for containers")
	_ = systemdCmd.RegisterFlagCompletionFunc(containerPrefixFlagName, completion.AutocompleteNone)

	podPrefixFlagName := "pod-prefix"
	flags.StringVar(&systemdOptions.PodPrefix, podPrefixFlagName, "pod", "Systemd unit name prefix for pods")
	_ = systemdCmd.RegisterFlagCompletionFunc(podPrefixFlagName, completion.AutocompleteNone)

	separatorFlagName := "separator"
	flags.StringVar(&systemdOptions.Separator, separatorFlagName, "-", "Systemd unit name separator between name/id and prefix")
	_ = systemdCmd.RegisterFlagCompletionFunc(separatorFlagName, completion.AutocompleteNone)

	flags.StringVar(&systemdRestart, restartPolicyFlagName, systemDefine.DefaultRestartPolicy, "Systemd restart-policy")
	_ = systemdCmd.RegisterFlagCompletionFunc(restartPolicyFlagName, common.AutocompleteSystemdRestartOptions)

	flags.UintVarP(&systemdRestartSec, restartSecFlagName, "", 0, "Systemd restart-sec")
	_ = systemdCmd.RegisterFlagCompletionFunc(restartSecFlagName, completion.AutocompleteNone)

	formatFlagName := "format"
	flags.StringVar(&format, formatFlagName, "", "Print the created units in specified format (json)")
	_ = systemdCmd.RegisterFlagCompletionFunc(formatFlagName, common.AutocompleteFormat(nil))

	flags.SetNormalizeFunc(utils.TimeoutAliasFlags)
}

func systemd(cmd *cobra.Command, args []string) error {
	if cmd.Flags().Changed(restartPolicyFlagName) {
		systemdOptions.RestartPolicy = &systemdRestart
	}

	if registry.IsRemote() {
		logrus.Warnln("The generated units should be placed on your remote system")
	}

	if cmd.Flags().Changed(newFlagName) && !systemdOptions.New && systemdOptions.TemplateUnitFile {
		return errors.New("--template cannot be set with --new=false")
	}
	if !systemdOptions.New && systemdOptions.TemplateUnitFile {
		systemdOptions.New = true
	}

	if cmd.Flags().Changed(restartSecFlagName) {
		systemdOptions.RestartSec = &systemdRestartSec
	}
	if cmd.Flags().Changed(startTimeoutFlagName) {
		systemdOptions.StartTimeout = &startTimeout
	}
	setStopTimeout := 0
	if cmd.Flags().Changed(stopTimeoutFlagName) {
		setStopTimeout++
	}
	if cmd.Flags().Changed(stopTimeoutCompatFlagName) {
		setStopTimeout++
	}
	switch setStopTimeout {
	case 1:
		systemdOptions.StopTimeout = &stopTimeout
	case 2:
		return fmt.Errorf("%s and %s are redundant and cannot be used together", stopTimeoutFlagName, stopTimeoutCompatFlagName)
	}

	reports, err := registry.ContainerEngine().GenerateSystemd(registry.GetContext(), args[0], systemdOptions)
	if err != nil {
		return err
	}

	if files {
		cwd, err := os.Getwd()
		if err != nil {
			return errors.Wrap(err, "error getting current working directory")
		}
		for name, content := range reports.Units {
			path := filepath.Join(cwd, fmt.Sprintf("%s.service", name))
			f, err := os.Create(path)
			if err != nil {
				return err
			}
			_, err = f.WriteString(content)
			if err != nil {
				return err
			}
			err = f.Close()
			if err != nil {
				return err
			}

			// add newline if default format is given
			if format == "" {
				path += "\n"
			}
			// modify in place so we can print the
			// paths when --files is set
			reports.Units[name] = path
		}
	}

	switch {
	case report.IsJSON(format):
		return printJSON(reports.Units)
	case format == "":
		return printDefault(reports.Units)
	default:
		return errors.Errorf("unknown --format argument: %s", format)
	}
}

func printDefault(units map[string]string) error {
	for _, content := range units {
		fmt.Print(content)
	}
	return nil
}

func printJSON(units map[string]string) error {
	b, err := json.MarshalIndent(units, "", " ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

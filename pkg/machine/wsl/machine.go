//go:build windows
// +build windows

package wsl

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/containers/podman/v4/pkg/machine"
	"github.com/containers/podman/v4/utils"
	"github.com/containers/storage/pkg/homedir"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

var (
	wslProvider = &Provider{}
	// vmtype refers to qemu (vs libvirt, krun, etc)
	vmtype = "wsl"
)

const (
	ErrorSuccessRebootInitiated = 1641
	ErrorSuccessRebootRequired  = 3010
)

// Usermode networking avoids potential nftables compatibility issues between the distro
// and the WSL Kernel. Additionally it avoids fw rule conflicts between distros, since
// all instances run under the same Kernel at runtime
const containersConf = `[containers]

[engine]
cgroup_manager = "cgroupfs"
events_logger = "file"
`

const appendPort = `grep -q Port\ %d /etc/ssh/sshd_config || echo Port %d >> /etc/ssh/sshd_config`

const configServices = `ln -fs /usr/lib/systemd/system/sshd.service /etc/systemd/system/multi-user.target.wants/sshd.service
ln -fs /usr/lib/systemd/system/podman.socket /etc/systemd/system/sockets.target.wants/podman.socket
rm -f /etc/systemd/system/getty.target.wants/console-getty.service
rm -f /etc/systemd/system/getty.target.wants/getty@tty1.service
rm -f /etc/systemd/system/multi-user.target.wants/systemd-resolved.service
rm -f /etc/systemd/system/dbus-org.freedesktop.resolve1.service
ln -fs /dev/null /etc/systemd/system/console-getty.service
mkdir -p /etc/systemd/system/systemd-sysusers.service.d/
adduser -m [USER] -G wheel
mkdir -p /home/[USER]/.config/systemd/[USER]/
chown [USER]:[USER] /home/[USER]/.config
`

const sudoers = `%wheel        ALL=(ALL)       NOPASSWD: ALL
`

const bootstrap = `#!/bin/bash
ps -ef | grep -v grep | grep -q systemd && exit 0
nohup unshare --kill-child --fork --pid --mount --mount-proc --propagation shared /lib/systemd/systemd >/dev/null 2>&1 &
sleep 0.1
`

const wslmotd = `
You will be automatically entered into a nested process namespace where
systemd is running. If you need to access the parent namespace, hit ctrl-d
or type exit. This also means to log out you need to exit twice.

`

const sysdpid = "SYSDPID=`ps -eo cmd,pid | grep -m 1 ^/lib/systemd/systemd | awk '{print $2}'`"

const profile = sysdpid + `
if [ ! -z "$SYSDPID" ] && [ "$SYSDPID" != "1" ]; then
    cat /etc/wslmotd
	/usr/local/bin/enterns
fi
`

const enterns = "#!/bin/bash\n" + sysdpid + `
if [ ! -z "$SYSDPID" ] && [ "$SYSDPID" != "1" ]; then
	nsenter -m -p -t $SYSDPID "$@"
fi
`

const waitTerm = sysdpid + `
if [ ! -z "$SYSDPID" ]; then
	timeout 60 tail -f /dev/null --pid $SYSDPID
fi
`

// WSL kernel does not have sg and crypto_user modules
const overrideSysusers = `[Service]
LoadCredential=
`

const lingerService = `[Unit]
Description=A systemd user unit demo
After=network-online.target
Wants=network-online.target podman.socket
[Service]
ExecStart=/usr/bin/sleep infinity
`

const lingerSetup = `mkdir -p /home/[USER]/.config/systemd/[USER]/default.target.wants
ln -fs /home/[USER]/.config/systemd/[USER]/linger-example.service \
       /home/[USER]/.config/systemd/[USER]/default.target.wants/linger-example.service
`

const wslInstallError = `Could not %s. See previous output for any potential failure details.
If you can not resolve the issue, and rerunning fails, try the "wsl --install" process
outlined in the following article:

http://docs.microsoft.com/en-us/windows/wsl/install

`

const wslKernelError = `Could not %s. See previous output for any potential failure details.
If you can not resolve the issue, try rerunning the "podman machine init command". If that fails
try the "wsl --update" command and then rerun "podman machine init". Finally, if all else fails,
try following the steps outlined in the following article:

http://docs.microsoft.com/en-us/windows/wsl/install

`

const wslInstallKernel = "install the WSL Kernel"

const wslOldVersion = `Automatic installation of WSL can not be performed on this version of Windows
Either update to Build 19041 (or later), or perform the manual installation steps
outlined in the following article:

http://docs.microsoft.com/en-us/windows/wsl/install\

`

type Provider struct{}

type MachineVM struct {
	// IdentityPath is the fq path to the ssh priv key
	IdentityPath string
	// IgnitionFilePath is the fq path to the .ign file
	ImageStream string
	// ImagePath is the fq path to
	ImagePath string
	// Name of the vm
	Name string
	// SSH port for user networking
	Port int
	// RemoteUsername of the vm user
	RemoteUsername string
}

type ExitCodeError struct {
	code uint
}

func (e *ExitCodeError) Error() string {
	return fmt.Sprintf("Process failed with exit code: %d", e.code)
}

func GetWSLProvider() machine.Provider {
	return wslProvider
}

// NewMachine initializes an instance of a virtual machine based on the qemu
// virtualization.
func (p *Provider) NewMachine(opts machine.InitOptions) (machine.VM, error) {
	vm := new(MachineVM)
	if len(opts.Name) > 0 {
		vm.Name = opts.Name
	}

	vm.ImagePath = opts.ImagePath
	vm.RemoteUsername = opts.Username

	// Add a random port for ssh
	port, err := utils.GetRandomPort()
	if err != nil {
		return nil, err
	}
	vm.Port = port

	return vm, nil
}

// LoadByName reads a json file that describes a known qemu vm
// and returns a vm instance
func (p *Provider) LoadVMByName(name string) (machine.VM, error) {
	vm := new(MachineVM)
	vmConfigDir, err := machine.GetConfDir(vmtype)
	if err != nil {
		return nil, err
	}
	b, err := ioutil.ReadFile(filepath.Join(vmConfigDir, name+".json"))
	if os.IsNotExist(err) {
		return nil, errors.Wrap(machine.ErrNoSuchVM, name)
	}
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(b, vm)
	return vm, err
}

// Init writes the json configuration file to the filesystem for
// other verbs (start, stop)
func (v *MachineVM) Init(opts machine.InitOptions) (bool, error) {
	if cont, err := checkAndInstallWSL(opts); !cont {
		appendOutputIfError(opts.ReExec, err)
		return cont, err
	}

	homeDir := homedir.Get()
	sshDir := filepath.Join(homeDir, ".ssh")
	v.IdentityPath = filepath.Join(sshDir, v.Name)

	if err := downloadDistro(v, opts); err != nil {
		return false, err
	}

	if err := writeJSON(v); err != nil {
		return false, err
	}

	if err := setupConnections(v, opts, sshDir); err != nil {
		return false, err
	}

	dist, err := provisionWSLDist(v)
	if err != nil {
		return false, err
	}

	fmt.Println("Configuring system...")
	if err = configureSystem(v, dist); err != nil {
		return false, err
	}

	if err = installScripts(dist); err != nil {
		return false, err
	}

	if err = createKeys(v, dist, sshDir); err != nil {
		return false, err
	}

	return true, nil
}

func downloadDistro(v *MachineVM, opts machine.InitOptions) error {
	var (
		dd  machine.DistributionDownload
		err error
	)

	if _, e := strconv.Atoi(opts.ImagePath); e == nil {
		v.ImageStream = opts.ImagePath
		dd, err = machine.NewFedoraDownloader(vmtype, v.Name, v.ImageStream)
	} else {
		v.ImageStream = "custom"
		dd, err = machine.NewGenericDownloader(vmtype, v.Name, opts.ImagePath)
	}
	if err != nil {
		return err
	}

	v.ImagePath = dd.Get().LocalUncompressedFile
	return machine.DownloadImage(dd)
}

func writeJSON(v *MachineVM) error {
	vmConfigDir, err := machine.GetConfDir(vmtype)
	if err != nil {
		return err
	}

	jsonFile := filepath.Join(vmConfigDir, v.Name) + ".json"

	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	if err := ioutil.WriteFile(jsonFile, b, 0644); err != nil {
		return errors.Wrap(err, "could not write machine json config")
	}

	return nil
}

func setupConnections(v *MachineVM, opts machine.InitOptions, sshDir string) error {
	uriRoot := machine.SSHRemoteConnection.MakeSSHURL("localhost", "/run/podman/podman.sock", strconv.Itoa(v.Port), "root")
	if err := machine.AddConnection(&uriRoot, v.Name+"-root", filepath.Join(sshDir, v.Name), opts.IsDefault); err != nil {
		return err
	}

	user := opts.Username
	uri := machine.SSHRemoteConnection.MakeSSHURL("localhost", withUser("/run/[USER]/1000/podman/podman.sock", user), strconv.Itoa(v.Port), v.RemoteUsername)
	return machine.AddConnection(&uri, v.Name, filepath.Join(sshDir, v.Name), opts.IsDefault)
}

func provisionWSLDist(v *MachineVM) (string, error) {
	vmDataDir, err := machine.GetDataDir(vmtype)
	if err != nil {
		return "", err
	}

	distDir := filepath.Join(vmDataDir, "wsldist")
	distTarget := filepath.Join(distDir, v.Name)
	if err := os.MkdirAll(distDir, 0755); err != nil {
		return "", errors.Wrap(err, "could not create wsldist directory")
	}

	dist := toDist(v.Name)
	fmt.Println("Importing operating system into WSL (this may take 5+ minutes on a new WSL install)...")
	if err = runCmdPassThrough("wsl", "--import", dist, distTarget, v.ImagePath); err != nil {
		return "", errors.Wrap(err, "WSL import of guest OS failed")
	}

	fmt.Println("Installing packages (this will take awhile)...")
	if err = runCmdPassThrough("wsl", "-d", dist, "dnf", "upgrade", "-y"); err != nil {
		return "", errors.Wrap(err, "package upgrade on guest OS failed")
	}

	if err = runCmdPassThrough("wsl", "-d", dist, "dnf", "install",
		"podman", "podman-docker", "openssh-server", "procps-ng", "-y"); err != nil {
		return "", errors.Wrap(err, "package installation on guest OS failed")
	}

	// Fixes newuidmap
	if err = runCmdPassThrough("wsl", "-d", dist, "dnf", "reinstall", "shadow-utils", "-y"); err != nil {
		return "", errors.Wrap(err, "package reinstallation of shadow-utils on guest OS failed")
	}

	// Windows 11 (NT Version = 10, Build 22000) generates harmless but scary messages on every
	// operation when mount was not present on the initial start. Force a cycle so that it won't
	// repeatedly complain.
	if winVersionAtLeast(10, 0, 22000) {
		if err := runCmdPassThrough("wsl", "--terminate", dist); err != nil {
			logrus.Warnf("could not cycle WSL dist: %s", err.Error())
		}
	}

	return dist, nil
}

func createKeys(v *MachineVM, dist string, sshDir string) error {
	user := v.RemoteUsername

	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return errors.Wrap(err, "could not create ssh directory")
	}

	if err := runCmdPassThrough("wsl", "--terminate", dist); err != nil {
		return errors.Wrap(err, "could not cycle WSL dist")
	}

	key, err := machine.CreateSSHKeysPrefix(sshDir, v.Name, true, true, "wsl", "-d", dist)
	if err != nil {
		return errors.Wrap(err, "could not create ssh keys")
	}

	if err := pipeCmdPassThrough("wsl", key+"\n", "-d", dist, "sh", "-c", "mkdir -p /root/.ssh;"+
		"cat >> /root/.ssh/authorized_keys; chmod 600 /root/.ssh/authorized_keys"); err != nil {
		return errors.Wrap(err, "could not create root authorized keys on guest OS")
	}

	userAuthCmd := withUser("mkdir -p /home/[USER]/.ssh;"+
		"cat >> /home/[USER]/.ssh/authorized_keys; chown -R [USER]:[USER] /home/[USER]/.ssh;"+
		"chmod 600 /home/[USER]/.ssh/authorized_keys", user)
	if err := pipeCmdPassThrough("wsl", key+"\n", "-d", dist, "sh", "-c", userAuthCmd); err != nil {
		return errors.Wrapf(err, "could not create '%s' authorized keys on guest OS", v.RemoteUsername)
	}

	return nil
}

func configureSystem(v *MachineVM, dist string) error {
	user := v.RemoteUsername
	if err := runCmdPassThrough("wsl", "-d", dist, "sh", "-c", fmt.Sprintf(appendPort, v.Port, v.Port)); err != nil {
		return errors.Wrap(err, "could not configure SSH port for guest OS")
	}

	if err := pipeCmdPassThrough("wsl", withUser(configServices, user), "-d", dist, "sh"); err != nil {
		return errors.Wrap(err, "could not configure systemd settings for guest OS")
	}

	if err := pipeCmdPassThrough("wsl", sudoers, "-d", dist, "sh", "-c", "cat >> /etc/sudoers"); err != nil {
		return errors.Wrap(err, "could not add wheel to sudoers")
	}

	if err := pipeCmdPassThrough("wsl", overrideSysusers, "-d", dist, "sh", "-c",
		"cat > /etc/systemd/system/systemd-sysusers.service.d/override.conf"); err != nil {
		return errors.Wrap(err, "could not generate systemd-sysusers override for guest OS")
	}

	lingerCmd := withUser("cat > /home/[USER]/.config/systemd/[USER]/linger-example.service", user)
	if err := pipeCmdPassThrough("wsl", lingerService, "-d", dist, "sh", "-c", lingerCmd); err != nil {
		return errors.Wrap(err, "could not generate linger service for guest OS")
	}

	if err := pipeCmdPassThrough("wsl", withUser(lingerSetup, user), "-d", dist, "sh"); err != nil {
		return errors.Wrap(err, "could not configure systemd settomgs for guest OS")
	}

	if err := pipeCmdPassThrough("wsl", containersConf, "-d", dist, "sh", "-c", "cat > /etc/containers/containers.conf"); err != nil {
		return errors.Wrap(err, "could not create containers.conf for guest OS")
	}

	return nil
}

func installScripts(dist string) error {
	if err := pipeCmdPassThrough("wsl", enterns, "-d", dist, "sh", "-c",
		"cat > /usr/local/bin/enterns; chmod 755 /usr/local/bin/enterns"); err != nil {
		return errors.Wrap(err, "could not create enterns script for guest OS")
	}

	if err := pipeCmdPassThrough("wsl", profile, "-d", dist, "sh", "-c",
		"cat > /etc/profile.d/enterns.sh"); err != nil {
		return errors.Wrap(err, "could not create motd profile script for guest OS")
	}

	if err := pipeCmdPassThrough("wsl", wslmotd, "-d", dist, "sh", "-c", "cat > /etc/wslmotd"); err != nil {
		return errors.Wrap(err, "could not create a WSL MOTD for guest OS")
	}

	if err := pipeCmdPassThrough("wsl", bootstrap, "-d", dist, "sh", "-c",
		"cat > /root/bootstrap; chmod 755 /root/bootstrap"); err != nil {
		return errors.Wrap(err, "could not create bootstrap script for guest OS")
	}

	return nil
}

func checkAndInstallWSL(opts machine.InitOptions) (bool, error) {
	if isWSLInstalled() {
		return true, nil
	}

	admin := hasAdminRights()

	if !isWSLFeatureEnabled() {
		return false, attemptFeatureInstall(opts, admin)
	}

	skip := false
	if !opts.ReExec && !admin {
		fmt.Println("Launching WSL Kernel Install...")
		if err := launchElevate(wslInstallKernel); err != nil {
			return false, err
		}

		skip = true
	}

	if !skip {
		if err := installWslKernel(); err != nil {
			fmt.Fprintf(os.Stderr, wslKernelError, wslInstallKernel)
			return false, err
		}

		if opts.ReExec {
			return false, nil
		}
	}

	return true, nil
}

func attemptFeatureInstall(opts machine.InitOptions, admin bool) error {
	if !winVersionAtLeast(10, 0, 18362) {
		return errors.Errorf("Your version of Windows does not support WSL. Update to Windows 10 Build 19041 or later")
	} else if !winVersionAtLeast(10, 0, 19041) {
		fmt.Fprint(os.Stderr, wslOldVersion)
		return errors.Errorf("WSL can not be automatically installed")
	}

	message := "WSL is not installed on this system, installing it.\n\n"

	if !admin {
		message += "Since you are not running as admin, a new window will open and " +
			"require you to approve administrator privileges.\n\n"
	}

	message += "NOTE: A system reboot will be required as part of this process. " +
		"If you prefer, you may abort now, and perform a manual installation using the \"wsl --install\" command."

	if !opts.ReExec && MessageBox(message, "Podman Machine", false) != 1 {
		return errors.Errorf("WSL installation aborted")
	}

	if !opts.ReExec && !admin {
		return launchElevate("install the Windows WSL Features")
	}

	return installWsl()
}

func launchElevate(operation string) error {
	truncateElevatedOutputFile()
	err := relaunchElevatedWait()
	if err != nil {
		if eerr, ok := err.(*ExitCodeError); ok {
			if eerr.code == ErrorSuccessRebootRequired {
				fmt.Println("Reboot is required to continue installation, please reboot at your convenience")
				return nil
			}
		}

		fmt.Fprintf(os.Stderr, "Elevated process failed with error: %v\n\n", err)
		dumpOutputFile()
		fmt.Fprintf(os.Stderr, wslInstallError, operation)
	}
	return err
}

func installWsl() error {
	log, err := getElevatedOutputFileWrite()
	if err != nil {
		return err
	}
	defer log.Close()
	if err := runCmdPassThroughTee(log, "dism", "/online", "/enable-feature",
		"/featurename:Microsoft-Windows-Subsystem-Linux", "/all", "/norestart"); isMsiError(err) {
		return errors.Wrap(err, "could not enable WSL Feature")
	}

	if err = runCmdPassThroughTee(log, "dism", "/online", "/enable-feature",
		"/featurename:VirtualMachinePlatform", "/all", "/norestart"); isMsiError(err) {
		return errors.Wrap(err, "could not enable Virtual Machine Feature")
	}
	log.Close()

	return reboot()
}

func installWslKernel() error {
	log, err := getElevatedOutputFileWrite()
	if err != nil {
		return err
	}
	defer log.Close()

	message := "Installing WSL Kernel Update"
	fmt.Println(message)
	fmt.Fprintln(log, message)

	backoff := 500 * time.Millisecond
	for i := 0; i < 5; i++ {
		err = runCmdPassThroughTee(log, "wsl", "--update")
		if err == nil {
			break
		}
		// In case of unusual circumstances (e.g. race with installer actions)
		// retry a few times
		message = "An error occured attempting the WSL Kernel update, retrying..."
		fmt.Println(message)
		fmt.Fprintln(log, message)
		time.Sleep(backoff)
		backoff *= 2
	}

	if err != nil {
		return errors.Wrap(err, "could not install WSL Kernel")
	}

	return nil
}

func getElevatedOutputFileName() (string, error) {
	dir, err := homedir.GetDataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "podman-elevated-output.log"), nil
}

func dumpOutputFile() {
	file, err := getElevatedOutputFileRead()
	if err != nil {
		logrus.Debug("could not find elevated child output file")
		return
	}
	defer file.Close()
	_, _ = io.Copy(os.Stdout, file)
}

func getElevatedOutputFileRead() (*os.File, error) {
	return getElevatedOutputFile(os.O_RDONLY)
}

func getElevatedOutputFileWrite() (*os.File, error) {
	return getElevatedOutputFile(os.O_WRONLY | os.O_CREATE | os.O_APPEND)
}

func appendOutputIfError(write bool, err error) {
	if write && err == nil {
		return
	}

	if file, check := getElevatedOutputFileWrite(); check == nil {
		defer file.Close()
		fmt.Fprintf(file, "Error: %v\n", err)
	}
}

func truncateElevatedOutputFile() error {
	name, err := getElevatedOutputFileName()
	if err != nil {
		return err
	}

	return os.Truncate(name, 0)
}

func getElevatedOutputFile(mode int) (*os.File, error) {
	name, err := getElevatedOutputFileName()
	if err != nil {
		return nil, err
	}

	dir, err := homedir.GetDataHome()
	if err != nil {
		return nil, err
	}

	if err = os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	return os.OpenFile(name, mode, 0644)
}

func isMsiError(err error) bool {
	if err == nil {
		return false
	}

	if eerr, ok := err.(*exec.ExitError); ok {
		switch eerr.ExitCode() {
		case 0:
			fallthrough
		case ErrorSuccessRebootInitiated:
			fallthrough
		case ErrorSuccessRebootRequired:
			return false
		}
	}

	return true
}
func toDist(name string) string {
	if !strings.HasPrefix(name, "podman") {
		name = "podman-" + name
	}
	return name
}

func withUser(s string, user string) string {
	return strings.ReplaceAll(s, "[USER]", user)
}

func runCmdPassThrough(name string, arg ...string) error {
	logrus.Debugf("Running command: %s %v", name, arg)
	cmd := exec.Command(name, arg...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runCmdPassThroughTee(out io.Writer, name string, arg ...string) error {
	logrus.Debugf("Running command: %s %v", name, arg)

	// TODO - Perhaps improve this with a conpty pseudo console so that
	//        dism installer text bars mirror console behavior (redraw)
	cmd := exec.Command(name, arg...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = io.MultiWriter(os.Stdout, out)
	cmd.Stderr = io.MultiWriter(os.Stderr, out)
	return cmd.Run()
}

func pipeCmdPassThrough(name string, input string, arg ...string) error {
	logrus.Debugf("Running command: %s %v", name, arg)
	cmd := exec.Command(name, arg...)
	cmd.Stdin = strings.NewReader(input)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (v *MachineVM) Start(name string, _ machine.StartOptions) error {
	if v.isRunning() {
		return errors.Errorf("%q is already running", name)
	}

	fmt.Println("Starting machine...")

	dist := toDist(name)

	err := runCmdPassThrough("wsl", "-d", dist, "/root/bootstrap")
	if err != nil {
		return errors.Wrap(err, "WSL bootstrap script failed")
	}

	return markStart(name)
}

func isWSLInstalled() bool {
	cmd := exec.Command("wsl", "--status")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return false
	}
	if err = cmd.Start(); err != nil {
		return false
	}
	scanner := bufio.NewScanner(transform.NewReader(out, unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder()))
	result := true
	for scanner.Scan() {
		line := scanner.Text()
		// Windows 11 does not set an error exit code when a kernel is not avail
		if strings.Contains(line, "kernel file is not found") {
			result = false
			break
		}
	}
	if err := cmd.Wait(); !result || err != nil {
		return false
	}

	return true
}

func isWSLFeatureEnabled() bool {
	cmd := exec.Command("wsl", "--set-default-version", "2")
	return cmd.Run() == nil
}

func isWSLRunning(dist string) (bool, error) {
	cmd := exec.Command("wsl", "-l", "--running")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return false, err
	}
	if err = cmd.Start(); err != nil {
		return false, err
	}
	scanner := bufio.NewScanner(transform.NewReader(out, unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder()))
	result := false
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 0 && dist == fields[0] {
			result = true
			break
		}
	}

	_ = cmd.Wait()

	return result, nil
}

func isSystemdRunning(dist string) (bool, error) {
	cmd := exec.Command("wsl", "-d", dist, "sh")
	cmd.Stdin = strings.NewReader(sysdpid + "\necho $SYSDPID\n")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return false, err
	}
	if err = cmd.Start(); err != nil {
		return false, err
	}
	scanner := bufio.NewScanner(out)
	result := false
	if scanner.Scan() {
		text := scanner.Text()
		i, err := strconv.Atoi(text)
		if err == nil && i > 0 {
			result = true
		}
	}

	_ = cmd.Wait()

	return result, nil
}

func (v *MachineVM) Stop(name string, _ machine.StopOptions) error {
	dist := toDist(v.Name)

	wsl, err := isWSLRunning(dist)
	if err != nil {
		return err
	}

	sysd := false
	if wsl {
		sysd, err = isSystemdRunning(dist)
		if err != nil {
			return err
		}
	}

	if !wsl || !sysd {
		return errors.Errorf("%q is not running", v.Name)
	}

	cmd := exec.Command("wsl", "-d", dist, "sh")
	cmd.Stdin = strings.NewReader(waitTerm)
	if err = cmd.Start(); err != nil {
		return errors.Wrap(err, "Error executing wait command")
	}

	exitCmd := exec.Command("wsl", "-d", dist, "/usr/local/bin/enterns", "systemctl", "exit", "0")
	if err = exitCmd.Run(); err != nil {
		return errors.Wrap(err, "Error stopping sysd")
	}

	if err = cmd.Wait(); err != nil {
		return err
	}

	cmd = exec.Command("wsl", "--terminate", dist)
	if err = cmd.Run(); err != nil {
		return err
	}

	return nil
}

//nolint:cyclop
func (v *MachineVM) Remove(name string, opts machine.RemoveOptions) (string, func() error, error) {
	var files []string

	if v.isRunning() {
		return "", nil, errors.Errorf("running vm %q cannot be destroyed", v.Name)
	}

	// Collect all the files that need to be destroyed
	if !opts.SaveKeys {
		files = append(files, v.IdentityPath, v.IdentityPath+".pub")
	}
	if !opts.SaveImage {
		files = append(files, v.ImagePath)
	}

	vmConfigDir, err := machine.GetConfDir(vmtype)
	if err != nil {
		return "", nil, err
	}
	files = append(files, filepath.Join(vmConfigDir, v.Name+".json"))

	vmDataDir, err := machine.GetDataDir(vmtype)
	if err != nil {
		return "", nil, err
	}
	files = append(files, filepath.Join(vmDataDir, "wsldist", v.Name))

	confirmationMessage := "\nThe following files will be deleted:\n\n"
	for _, msg := range files {
		confirmationMessage += msg + "\n"
	}

	confirmationMessage += "\n"
	return confirmationMessage, func() error {
		if err := machine.RemoveConnection(v.Name); err != nil {
			logrus.Error(err)
		}
		if err := machine.RemoveConnection(v.Name + "-root"); err != nil {
			logrus.Error(err)
		}
		if err := runCmdPassThrough("wsl", "--unregister", toDist(v.Name)); err != nil {
			logrus.Error(err)
		}
		for _, f := range files {
			if err := os.RemoveAll(f); err != nil {
				logrus.Error(err)
			}
		}
		return nil
	}, nil
}

func (v *MachineVM) isRunning() bool {
	dist := toDist(v.Name)

	wsl, err := isWSLRunning(dist)
	if err != nil {
		return false
	}

	sysd := false
	if wsl {
		sysd, err = isSystemdRunning(dist)

		if err != nil {
			return false
		}
	}

	return sysd
}

// SSH opens an interactive SSH session to the vm specified.
// Added ssh function to VM interface: pkg/machine/config/go : line 58
func (v *MachineVM) SSH(name string, opts machine.SSHOptions) error {
	if !v.isRunning() {
		return errors.Errorf("vm %q is not running.", v.Name)
	}

	username := opts.Username
	if username == "" {
		username = v.RemoteUsername
	}

	sshDestination := username + "@localhost"
	port := strconv.Itoa(v.Port)

	args := []string{"-i", v.IdentityPath, "-p", port, sshDestination, "-o", "UserKnownHostsFile /dev/null", "-o", "StrictHostKeyChecking no"}
	if len(opts.Args) > 0 {
		args = append(args, opts.Args...)
	} else {
		fmt.Printf("Connecting to vm %s. To close connection, use `~.` or `exit`\n", v.Name)
	}

	cmd := exec.Command("ssh", args...)
	logrus.Debugf("Executing: ssh %v\n", args)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	return cmd.Run()
}

// List lists all vm's that use qemu virtualization
func (p *Provider) List(_ machine.ListOptions) ([]*machine.ListResponse, error) {
	return GetVMInfos()
}

func GetVMInfos() ([]*machine.ListResponse, error) {
	vmConfigDir, err := machine.GetConfDir(vmtype)
	if err != nil {
		return nil, err
	}

	var listed []*machine.ListResponse

	if err = filepath.Walk(vmConfigDir, func(path string, info os.FileInfo, err error) error {
		vm := new(MachineVM)
		if strings.HasSuffix(info.Name(), ".json") {
			fullPath := filepath.Join(vmConfigDir, info.Name())
			b, err := ioutil.ReadFile(fullPath)
			if err != nil {
				return err
			}
			err = json.Unmarshal(b, vm)
			if err != nil {
				return err
			}
			listEntry := new(machine.ListResponse)

			listEntry.Name = vm.Name
			listEntry.Stream = vm.ImageStream
			listEntry.VMType = "wsl"
			listEntry.CPUs, _ = getCPUs(vm)
			listEntry.Memory, _ = getMem(vm)
			listEntry.DiskSize = getDiskSize(vm)
			fi, err := os.Stat(fullPath)
			if err != nil {
				return err
			}
			listEntry.CreatedAt = fi.ModTime()
			listEntry.LastUp = getLastStart(vm, fi.ModTime())
			if vm.isRunning() {
				listEntry.Running = true
			}

			listed = append(listed, listEntry)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return listed, err
}

func getDiskSize(vm *MachineVM) uint64 {
	vmDataDir, err := machine.GetDataDir(vmtype)
	if err != nil {
		return 0
	}
	distDir := filepath.Join(vmDataDir, "wsldist")
	disk := filepath.Join(distDir, vm.Name, "ext4.vhdx")
	info, err := os.Stat(disk)
	if err != nil {
		return 0
	}
	return uint64(info.Size())
}

func markStart(name string) error {
	vmDataDir, err := machine.GetDataDir(vmtype)
	if err != nil {
		return err
	}
	distDir := filepath.Join(vmDataDir, "wsldist")
	start := filepath.Join(distDir, name, "laststart")
	file, err := os.Create(start)
	if err != nil {
		return err
	}
	file.Close()

	return nil
}

func getLastStart(vm *MachineVM, created time.Time) time.Time {
	vmDataDir, err := machine.GetDataDir(vmtype)
	if err != nil {
		return created
	}
	distDir := filepath.Join(vmDataDir, "wsldist")
	start := filepath.Join(distDir, vm.Name, "laststart")
	info, err := os.Stat(start)
	if err != nil {
		return created
	}
	return info.ModTime()
}

func getCPUs(vm *MachineVM) (uint64, error) {
	dist := toDist(vm.Name)
	if run, _ := isWSLRunning(dist); !run {
		return 0, nil
	}
	cmd := exec.Command("wsl", "-d", dist, "nproc")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	if err = cmd.Start(); err != nil {
		return 0, err
	}
	scanner := bufio.NewScanner(out)
	var result string
	for scanner.Scan() {
		result = scanner.Text()
	}
	_ = cmd.Wait()

	ret, err := strconv.Atoi(result)
	return uint64(ret), err
}

func getMem(vm *MachineVM) (uint64, error) {
	dist := toDist(vm.Name)
	if run, _ := isWSLRunning(dist); !run {
		return 0, nil
	}
	cmd := exec.Command("wsl", "-d", dist, "cat", "/proc/meminfo")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	if err = cmd.Start(); err != nil {
		return 0, err
	}
	scanner := bufio.NewScanner(out)
	var (
		total, available uint64
		t, a             int
	)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if strings.HasPrefix(fields[0], "MemTotal") && len(fields) >= 2 {
			t, err = strconv.Atoi(fields[1])
			total = uint64(t) * 1024
		} else if strings.HasPrefix(fields[0], "MemAvailable") && len(fields) >= 2 {
			a, err = strconv.Atoi(fields[1])
			available = uint64(a) * 1024
		}
		if err != nil {
			break
		}
	}
	_ = cmd.Wait()

	return total - available, err
}

func (p *Provider) IsValidVMName(name string) (bool, error) {
	infos, err := GetVMInfos()
	if err != nil {
		return false, err
	}
	for _, vm := range infos {
		if vm.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func (p *Provider) CheckExclusiveActiveVM() (bool, string, error) {
	return false, "", nil
}

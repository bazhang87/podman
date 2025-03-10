//go:build amd64 || arm64
// +build amd64 arm64

package machine

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"

	"github.com/containers/common/pkg/config"
	"github.com/sirupsen/logrus"
)

/*
	If this file gets too nuts, we can perhaps use existing go code
	to create ignition files.  At this point, the file is so simple
	that I chose to use structs and not import any code as I was
	concerned (unsubstantiated) about too much bloat coming in.

	https://github.com/openshift/machine-config-operator/blob/master/pkg/server/server.go
*/

// Convenience function to convert int to ptr
func intToPtr(i int) *int {
	return &i
}

// Convenience function to convert string to ptr
func strToPtr(s string) *string {
	return &s
}

// Convenience function to convert bool to ptr
func boolToPtr(b bool) *bool {
	return &b
}

func getNodeUsr(usrName string) NodeUser {
	return NodeUser{Name: &usrName}
}

func getNodeGrp(grpName string) NodeGroup {
	return NodeGroup{Name: &grpName}
}

type DynamicIgnition struct {
	Name      string
	Key       string
	TimeZone  string
	VMName    string
	WritePath string
}

// NewIgnitionFile
func NewIgnitionFile(ign DynamicIgnition) error {
	if len(ign.Name) < 1 {
		ign.Name = DefaultIgnitionUserName
	}
	ignVersion := Ignition{
		Version: "3.2.0",
	}

	ignPassword := Passwd{
		Users: []PasswdUser{
			{
				Name:              ign.Name,
				SSHAuthorizedKeys: []SSHAuthorizedKey{SSHAuthorizedKey(ign.Key)},
			},
			{
				Name:              "root",
				SSHAuthorizedKeys: []SSHAuthorizedKey{SSHAuthorizedKey(ign.Key)},
			},
		},
	}

	ignStorage := Storage{
		Directories: getDirs(ign.Name),
		Files:       getFiles(ign.Name),
		Links:       getLinks(ign.Name),
	}

	// Add or set the time zone for the machine
	if len(ign.TimeZone) > 0 {
		var (
			err error
			tz  string
		)
		// local means the same as the host
		// lookup where it is pointing to on the host
		if ign.TimeZone == "local" {
			tz, err = getLocalTimeZone()
			if err != nil {
				return err
			}
		} else {
			tz = ign.TimeZone
		}
		tzLink := Link{
			Node: Node{
				Group:     getNodeGrp("root"),
				Path:      "/etc/localtime",
				Overwrite: boolToPtr(false),
				User:      getNodeUsr("root"),
			},
			LinkEmbedded1: LinkEmbedded1{
				Hard:   boolToPtr(false),
				Target: filepath.Join("/usr/share/zoneinfo", tz),
			},
		}
		ignStorage.Links = append(ignStorage.Links, tzLink)
	}

	// ready is a unit file that sets up the virtual serial device
	// where when the VM is done configuring, it will send an ack
	// so a listening host knows it can being interacting with it
	ready := `[Unit]
Requires=dev-virtio\\x2dports-%s.device
After=remove-moby.service sshd.socket sshd.service
OnFailure=emergency.target
OnFailureJobMode=isolate
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c '/usr/bin/echo Ready >/dev/%s'
[Install]
RequiredBy=default.target
`
	deMoby := `[Unit]
Description=Remove moby-engine
# Run once for the machine
After=systemd-machine-id-commit.service
Before=zincati.service
ConditionPathExists=!/var/lib/%N.stamp

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/bin/rpm-ostree override remove moby-engine
ExecStart=/usr/bin/rpm-ostree ex apply-live --allow-replacement
ExecStartPost=/bin/touch /var/lib/%N.stamp

[Install]
WantedBy=default.target
 `
	_ = ready
	ignSystemd := Systemd{
		Units: []Unit{
			{
				Enabled: boolToPtr(true),
				Name:    "podman.socket",
			},
			{
				Enabled:  boolToPtr(true),
				Name:     "ready.service",
				Contents: strToPtr(fmt.Sprintf(ready, "vport1p1", "vport1p1")),
			},
			{
				Enabled: boolToPtr(false),
				Name:    "docker.service",
				Mask:    boolToPtr(true),
			},
			{
				Enabled: boolToPtr(false),
				Name:    "docker.socket",
				Mask:    boolToPtr(true),
			},
			{
				Enabled:  boolToPtr(true),
				Name:     "remove-moby.service",
				Contents: &deMoby,
			},
		}}
	ignConfig := Config{
		Ignition: ignVersion,
		Passwd:   ignPassword,
		Storage:  ignStorage,
		Systemd:  ignSystemd,
	}
	b, err := json.Marshal(ignConfig)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(ign.WritePath, b, 0644)
}

func getDirs(usrName string) []Directory {
	// Ignition has a bug/feature? where if you make a series of dirs
	// in one swoop, then the leading dirs are creates as root.
	newDirs := []string{
		"/home/" + usrName + "/.config",
		"/home/" + usrName + "/.config/containers",
		"/home/" + usrName + "/.config/systemd",
		"/home/" + usrName + "/.config/systemd/user",
		"/home/" + usrName + "/.config/systemd/user/default.target.wants",
	}
	var (
		dirs = make([]Directory, len(newDirs))
	)
	for i, d := range newDirs {
		newDir := Directory{
			Node: Node{
				Group: getNodeGrp(usrName),
				Path:  d,
				User:  getNodeUsr(usrName),
			},
			DirectoryEmbedded1: DirectoryEmbedded1{Mode: intToPtr(0755)},
		}
		dirs[i] = newDir
	}

	// Issue #11489: make sure that we can inject a custom registries.conf
	// file on the system level to force a single search registry.
	// The remote client does not yet support prompting for short-name
	// resolution, so we enforce a single search registry (i.e., docker.io)
	// as a workaround.
	dirs = append(dirs, Directory{
		Node: Node{
			Group: getNodeGrp("root"),
			Path:  "/etc/containers/registries.conf.d",
			User:  getNodeUsr("root"),
		},
		DirectoryEmbedded1: DirectoryEmbedded1{Mode: intToPtr(0755)},
	})

	return dirs
}

func getFiles(usrName string) []File {
	var (
		files []File
	)

	lingerExample := `[Unit]
Description=A systemd user unit demo
After=network-online.target
Wants=network-online.target podman.socket
[Service]
ExecStart=/usr/bin/sleep infinity
`
	containers := `[containers]
netns="bridge"
rootless_networking="cni"
`
	rootContainers := `[engine]
machine_enabled=true
`

	// Add a fake systemd service to get the user socket rolling
	files = append(files, File{
		Node: Node{
			Group: getNodeGrp(usrName),
			Path:  "/home/" + usrName + "/.config/systemd/user/linger-example.service",
			User:  getNodeUsr(usrName),
		},
		FileEmbedded1: FileEmbedded1{
			Append: nil,
			Contents: Resource{
				Source: encodeDataURLPtr(lingerExample),
			},
			Mode: intToPtr(0744),
		},
	})

	// Set containers.conf up for core user to use cni networks
	// by default
	files = append(files, File{
		Node: Node{
			Group: getNodeGrp(usrName),
			Path:  "/home/" + usrName + "/.config/containers/containers.conf",
			User:  getNodeUsr(usrName),
		},
		FileEmbedded1: FileEmbedded1{
			Append: nil,
			Contents: Resource{
				Source: encodeDataURLPtr(containers),
			},
			Mode: intToPtr(0744),
		},
	})
	// Add a file into linger
	files = append(files, File{
		Node: Node{
			Group: getNodeGrp(usrName),
			Path:  "/var/lib/systemd/linger/core",
			User:  getNodeUsr(usrName),
		},
		FileEmbedded1: FileEmbedded1{Mode: intToPtr(0644)},
	})

	// Set machine_enabled to true to indicate we're in a VM
	files = append(files, File{
		Node: Node{
			Group: getNodeGrp("root"),
			Path:  "/etc/containers/containers.conf",
			User:  getNodeUsr("root"),
		},
		FileEmbedded1: FileEmbedded1{
			Append: nil,
			Contents: Resource{
				Source: encodeDataURLPtr(rootContainers),
			},
			Mode: intToPtr(0644),
		},
	})

	// Issue #11489: make sure that we can inject a custom registries.conf
	// file on the system level to force a single search registry.
	// The remote client does not yet support prompting for short-name
	// resolution, so we enforce a single search registry (i.e., docker.io)
	// as a workaround.
	files = append(files, File{
		Node: Node{
			Group: getNodeGrp("root"),
			Path:  "/etc/containers/registries.conf.d/999-podman-machine.conf",
			User:  getNodeUsr("root"),
		},
		FileEmbedded1: FileEmbedded1{
			Append: nil,
			Contents: Resource{
				Source: encodeDataURLPtr("unqualified-search-registries=[\"docker.io\"]\n"),
			},
			Mode: intToPtr(0644),
		},
	})

	files = append(files, File{
		Node: Node{
			Path: "/etc/tmpfiles.d/podman-docker.conf",
		},
		FileEmbedded1: FileEmbedded1{
			Append: nil,
			// Create a symlink from the docker socket to the podman socket.
			// Taken from https://github.com/containers/podman/blob/main/contrib/systemd/system/podman-docker.conf
			Contents: Resource{
				Source: encodeDataURLPtr("L+  /run/docker.sock   -    -    -     -   /run/podman/podman.sock\n"),
			},
			Mode: intToPtr(0644),
		},
	})

	setProxyOpts := getProxyVariables()
	if setProxyOpts != "" {
		files = append(files, File{
			Node: Node{
				Group: getNodeGrp("root"),
				Path:  "/etc/profile.d/proxy-opts.sh",
				User:  getNodeUsr("root"),
			},
			FileEmbedded1: FileEmbedded1{
				Append: nil,
				Contents: Resource{
					Source: encodeDataURLPtr(setProxyOpts),
				},
				Mode: intToPtr(0644),
			},
		})
	}

	setDockerHost := `export DOCKER_HOST="unix://$(podman info -f "{{.Host.RemoteSocket.Path}}")"
`

	files = append(files, File{
		Node: Node{
			Group: getNodeGrp("root"),
			Path:  "/etc/profile.d/docker-host.sh",
			User:  getNodeUsr("root"),
		},
		FileEmbedded1: FileEmbedded1{
			Append: nil,
			Contents: Resource{
				Source: encodeDataURLPtr(setDockerHost),
			},
			Mode: intToPtr(0644),
		},
	})

	// get certs for current user
	userHome, err := os.UserHomeDir()
	if err != nil {
		logrus.Warnf("Unable to copy certs via ignition %s", err.Error())
		return files
	}

	certFiles := getCerts(filepath.Join(userHome, ".config/containers/certs.d"), true)
	files = append(files, certFiles...)

	certFiles = getCerts(filepath.Join(userHome, ".config/docker/certs.d"), true)
	files = append(files, certFiles...)

	if sslCertFile, ok := os.LookupEnv("SSL_CERT_FILE"); ok {
		if _, err := os.Stat(sslCertFile); err == nil {
			certFiles = getCerts(sslCertFile, false)
			files = append(files, certFiles...)

			if len(certFiles) > 0 {
				setSSLCertFile := fmt.Sprintf("export %s=%s", "SSL_CERT_FILE", filepath.Join("/etc/containers/certs.d", filepath.Base(sslCertFile)))
				files = append(files, File{
					Node: Node{
						Group: getNodeGrp("root"),
						Path:  "/etc/profile.d/ssl_cert_file.sh",
						User:  getNodeUsr("root"),
					},
					FileEmbedded1: FileEmbedded1{
						Append: nil,
						Contents: Resource{
							Source: encodeDataURLPtr(setSSLCertFile),
						},
						Mode: intToPtr(0644),
					},
				})
			}
		}
	}

	return files
}

func getCerts(certsDir string, isDir bool) []File {
	var (
		files []File
	)

	if isDir {
		err := filepath.Walk(certsDir, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				certPath, err := filepath.Rel(certsDir, path)
				if err != nil {
					logrus.Warnf("%s", err)
					return nil
				}

				file, err := prepareCertFile(filepath.Join(certsDir, certPath), certPath)
				if err == nil {
					files = append(files, file)
				}
			}

			return nil
		})
		if err != nil {
			if !os.IsNotExist(err) {
				logrus.Warnf("Unable to copy certs via ignition, error while reading certs from %s:  %s", certsDir, err.Error())
			}
		}
	} else {
		fileName := filepath.Base(certsDir)
		file, err := prepareCertFile(certsDir, fileName)
		if err == nil {
			files = append(files, file)
		}
	}

	return files
}

func prepareCertFile(path string, name string) (File, error) {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		logrus.Warnf("Unable to read cert file %s", err.Error())
		return File{}, err
	}

	targetPath := filepath.Join("/etc/containers/certs.d", name)

	logrus.Debugf("Copying cert file from '%s' to '%s'.", path, targetPath)

	file := File{
		Node: Node{
			Group: getNodeGrp("root"),
			Path:  targetPath,
			User:  getNodeUsr("root"),
		},
		FileEmbedded1: FileEmbedded1{
			Append: nil,
			Contents: Resource{
				Source: encodeDataURLPtr(string(b)),
			},
			Mode: intToPtr(0644),
		},
	}
	return file, nil
}

func getProxyVariables() string {
	proxyOpts := ""
	for _, variable := range config.ProxyEnv {
		if value, ok := os.LookupEnv(variable); ok {
			proxyOpts += fmt.Sprintf("\n export %s=%s", variable, value)
		}
	}
	return proxyOpts
}

func getLinks(usrName string) []Link {
	return []Link{{
		Node: Node{
			Group: getNodeGrp(usrName),
			Path:  "/home/" + usrName + "/.config/systemd/user/default.target.wants/linger-example.service",
			User:  getNodeUsr(usrName),
		},
		LinkEmbedded1: LinkEmbedded1{
			Hard:   boolToPtr(false),
			Target: "/home/" + usrName + "/.config/systemd/user/linger-example.service",
		},
	}, {
		Node: Node{
			Group:     getNodeGrp("root"),
			Path:      "/usr/local/bin/docker",
			Overwrite: boolToPtr(true),
			User:      getNodeUsr("root"),
		},
		LinkEmbedded1: LinkEmbedded1{
			Hard:   boolToPtr(false),
			Target: "/usr/bin/podman",
		},
	}}
}

func encodeDataURLPtr(contents string) *string {
	return strToPtr(fmt.Sprintf("data:,%s", url.PathEscape(contents)))
}

package parse

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/containers/buildah/internal"
	internalUtil "github.com/containers/buildah/internal/util"
	"github.com/containers/common/pkg/parse"
	"github.com/containers/image/v5/types"
	"github.com/containers/storage"
	"github.com/containers/storage/pkg/idtools"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
)

const (
	// TypeBind is the type for mounting host dir
	TypeBind = "bind"
	// TypeTmpfs is the type for mounting tmpfs
	TypeTmpfs = "tmpfs"
	// TypeCache is the type for mounting a common persistent cache from host
	TypeCache = "cache"
	// mount=type=cache must create a persistent directory on host so its available for all consecutive builds.
	// Lifecycle of following directory will be inherited from how host machine treats temporary directory
	BuildahCacheDir = "buildah-cache"
)

var (
	errBadMntOption = errors.New("invalid mount option")
	errBadOptionArg = errors.New("must provide an argument for option")
	errBadVolDest   = errors.New("must set volume destination")
	errBadVolSrc    = errors.New("must set volume source")
)

// GetBindMount parses a single bind mount entry from the --mount flag.
// Returns specifiedMount and a string which contains name of image that we mounted otherwise its empty.
// Caller is expected to perform unmount of any mounted images
func GetBindMount(ctx *types.SystemContext, args []string, contextDir string, store storage.Store, imageMountLabel string, additionalMountPoints map[string]internal.StageMountDetails) (specs.Mount, string, error) {
	newMount := specs.Mount{
		Type: TypeBind,
	}

	mountReadability := false
	setDest := false
	bindNonRecursive := false
	fromImage := ""

	for _, val := range args {
		kv := strings.SplitN(val, "=", 2)
		switch kv[0] {
		case "bind-nonrecursive":
			newMount.Options = append(newMount.Options, "bind")
			bindNonRecursive = true
		case "ro", "nosuid", "nodev", "noexec":
			// TODO: detect duplication of these options.
			// (Is this necessary?)
			newMount.Options = append(newMount.Options, kv[0])
			mountReadability = true
		case "rw", "readwrite":
			newMount.Options = append(newMount.Options, "rw")
			mountReadability = true
		case "readonly":
			// Alias for "ro"
			newMount.Options = append(newMount.Options, "ro")
			mountReadability = true
		case "shared", "rshared", "private", "rprivate", "slave", "rslave", "Z", "z", "U":
			newMount.Options = append(newMount.Options, kv[0])
		case "from":
			if len(kv) == 1 {
				return newMount, "", errors.Wrapf(errBadOptionArg, kv[0])
			}
			fromImage = kv[1]
		case "bind-propagation":
			if len(kv) == 1 {
				return newMount, "", errors.Wrapf(errBadOptionArg, kv[0])
			}
			newMount.Options = append(newMount.Options, kv[1])
		case "src", "source":
			if len(kv) == 1 {
				return newMount, "", errors.Wrapf(errBadOptionArg, kv[0])
			}
			newMount.Source = kv[1]
		case "target", "dst", "destination":
			if len(kv) == 1 {
				return newMount, "", errors.Wrapf(errBadOptionArg, kv[0])
			}
			if err := parse.ValidateVolumeCtrDir(kv[1]); err != nil {
				return newMount, "", err
			}
			newMount.Destination = kv[1]
			setDest = true
		case "consistency":
			// Option for OS X only, has no meaning on other platforms
			// and can thus be safely ignored.
			// See also the handling of the equivalent "delegated" and "cached" in ValidateVolumeOpts
		default:
			return newMount, "", errors.Wrapf(errBadMntOption, kv[0])
		}
	}

	// default mount readability is always readonly
	if !mountReadability {
		newMount.Options = append(newMount.Options, "ro")
	}

	// Following variable ensures that we return imagename only if we did additional mount
	isImageMounted := false
	if fromImage != "" {
		mountPoint := ""
		if additionalMountPoints != nil {
			if val, ok := additionalMountPoints[fromImage]; ok {
				mountPoint = val.MountPoint
			}
		}
		// if mountPoint of image was not found in additionalMap
		// or additionalMap was nil, try mounting image
		if mountPoint == "" {
			image, err := internalUtil.LookupImage(ctx, store, fromImage)
			if err != nil {
				return newMount, "", err
			}

			mountPoint, err = image.Mount(context.Background(), nil, imageMountLabel)
			if err != nil {
				return newMount, "", err
			}
			isImageMounted = true
		}
		contextDir = mountPoint
	}

	// buildkit parity: default bind option must be `rbind`
	// unless specified
	if !bindNonRecursive {
		newMount.Options = append(newMount.Options, "rbind")
	}

	if !setDest {
		return newMount, fromImage, errBadVolDest
	}

	// buildkit parity: support absolute path for sources from current build context
	if contextDir != "" {
		// path should be /contextDir/specified path
		newMount.Source = filepath.Join(contextDir, filepath.Clean(string(filepath.Separator)+newMount.Source))
	} else {
		// looks like its coming from `build run --mount=type=bind` allow using absolute path
		// error out if no source is set
		if newMount.Source == "" {
			return newMount, "", errBadVolSrc
		}
		if err := parse.ValidateVolumeHostDir(newMount.Source); err != nil {
			return newMount, "", err
		}
	}

	opts, err := parse.ValidateVolumeOpts(newMount.Options)
	if err != nil {
		return newMount, fromImage, err
	}
	newMount.Options = opts

	if !isImageMounted {
		// we don't want any cleanups if image was not mounted explicitly
		// so dont return anything
		fromImage = ""
	}

	return newMount, fromImage, nil
}

// GetCacheMount parses a single cache mount entry from the --mount flag.
func GetCacheMount(args []string, store storage.Store, imageMountLabel string, additionalMountPoints map[string]internal.StageMountDetails) (specs.Mount, error) {
	var err error
	var mode uint64
	var (
		setDest     bool
		setShared   bool
		setReadOnly bool
	)
	fromStage := ""
	newMount := specs.Mount{
		Type: TypeBind,
	}
	// if id is set a new subdirectory with `id` will be created under /host-temp/buildah-build-cache/id
	id := ""
	//buidkit parity: cache directory defaults to 755
	mode = 0o755
	//buidkit parity: cache directory defaults to uid 0 if not specified
	uid := 0
	//buidkit parity: cache directory defaults to gid 0 if not specified
	gid := 0

	for _, val := range args {
		kv := strings.SplitN(val, "=", 2)
		switch kv[0] {
		case "nosuid", "nodev", "noexec":
			// TODO: detect duplication of these options.
			// (Is this necessary?)
			newMount.Options = append(newMount.Options, kv[0])
		case "rw", "readwrite":
			newMount.Options = append(newMount.Options, "rw")
		case "readonly", "ro":
			// Alias for "ro"
			newMount.Options = append(newMount.Options, "ro")
			setReadOnly = true
		case "shared", "rshared", "private", "rprivate", "slave", "rslave", "Z", "z", "U":
			newMount.Options = append(newMount.Options, kv[0])
			setShared = true
		case "bind-propagation":
			if len(kv) == 1 {
				return newMount, errors.Wrapf(errBadOptionArg, kv[0])
			}
			newMount.Options = append(newMount.Options, kv[1])
		case "id":
			if len(kv) == 1 {
				return newMount, errors.Wrapf(errBadOptionArg, kv[0])
			}
			id = kv[1]
		case "from":
			if len(kv) == 1 {
				return newMount, errors.Wrapf(errBadOptionArg, kv[0])
			}
			fromStage = kv[1]
		case "target", "dst", "destination":
			if len(kv) == 1 {
				return newMount, errors.Wrapf(errBadOptionArg, kv[0])
			}
			if err := parse.ValidateVolumeCtrDir(kv[1]); err != nil {
				return newMount, err
			}
			newMount.Destination = kv[1]
			setDest = true
		case "src", "source":
			if len(kv) == 1 {
				return newMount, errors.Wrapf(errBadOptionArg, kv[0])
			}
			newMount.Source = kv[1]
		case "mode":
			if len(kv) == 1 {
				return newMount, errors.Wrapf(errBadOptionArg, kv[0])
			}
			mode, err = strconv.ParseUint(kv[1], 8, 32)
			if err != nil {
				return newMount, errors.Wrapf(err, "Unable to parse cache mode")
			}
		case "uid":
			if len(kv) == 1 {
				return newMount, errors.Wrapf(errBadOptionArg, kv[0])
			}
			uid, err = strconv.Atoi(kv[1])
			if err != nil {
				return newMount, errors.Wrapf(err, "Unable to parse cache uid")
			}
		case "gid":
			if len(kv) == 1 {
				return newMount, errors.Wrapf(errBadOptionArg, kv[0])
			}
			gid, err = strconv.Atoi(kv[1])
			if err != nil {
				return newMount, errors.Wrapf(err, "Unable to parse cache gid")
			}
		default:
			return newMount, errors.Wrapf(errBadMntOption, kv[0])
		}
	}

	if !setDest {
		return newMount, errBadVolDest
	}

	if fromStage != "" {
		// do not create cache on host
		// instead use read-only mounted stage as cache
		mountPoint := ""
		if additionalMountPoints != nil {
			if val, ok := additionalMountPoints[fromStage]; ok {
				if val.IsStage {
					mountPoint = val.MountPoint
				}
			}
		}
		// Cache does not supports using image so if not stage found
		// return with error
		if mountPoint == "" {
			return newMount, fmt.Errorf("no stage found with name %s", fromStage)
		}
		// path should be /contextDir/specified path
		newMount.Source = filepath.Join(mountPoint, filepath.Clean(string(filepath.Separator)+newMount.Source))
	} else {
		// we need to create cache on host if no image is being used

		// since type is cache and cache can be reused by consecutive builds
		// create a common cache directory, which persists on hosts within temp lifecycle
		// add subdirectory if specified

		// cache parent directory
		cacheParent := filepath.Join(getTempDir(), BuildahCacheDir)
		// create cache on host if not present
		err = os.MkdirAll(cacheParent, os.FileMode(0755))
		if err != nil {
			return newMount, errors.Wrapf(err, "Unable to create build cache directory")
		}

		if id != "" {
			newMount.Source = filepath.Join(cacheParent, filepath.Clean(id))
		} else {
			newMount.Source = filepath.Join(cacheParent, filepath.Clean(newMount.Destination))
		}
		idPair := idtools.IDPair{
			UID: uid,
			GID: gid,
		}
		//buildkit parity: change uid and gid if specificed otheriwise keep `0`
		err = idtools.MkdirAllAndChownNew(newMount.Source, os.FileMode(mode), idPair)
		if err != nil {
			return newMount, errors.Wrapf(err, "Unable to change uid,gid of cache directory")
		}
	}

	// buildkit parity: default sharing should be shared
	// unless specified
	if !setShared {
		newMount.Options = append(newMount.Options, "shared")
	}

	// buildkit parity: cache must writable unless `ro` or `readonly` is configured explicitly
	if !setReadOnly {
		newMount.Options = append(newMount.Options, "rw")
	}

	newMount.Options = append(newMount.Options, "bind")

	opts, err := parse.ValidateVolumeOpts(newMount.Options)
	if err != nil {
		return newMount, err
	}
	newMount.Options = opts

	return newMount, nil
}

// GetTmpfsMount parses a single tmpfs mount entry from the --mount flag
func GetTmpfsMount(args []string) (specs.Mount, error) {
	newMount := specs.Mount{
		Type:   TypeTmpfs,
		Source: TypeTmpfs,
	}

	setDest := false

	for _, val := range args {
		kv := strings.SplitN(val, "=", 2)
		switch kv[0] {
		case "ro", "nosuid", "nodev", "noexec":
			newMount.Options = append(newMount.Options, kv[0])
		case "readonly":
			// Alias for "ro"
			newMount.Options = append(newMount.Options, "ro")
		case "tmpcopyup":
			//the path that is shadowed by the tmpfs mount is recursively copied up to the tmpfs itself.
			newMount.Options = append(newMount.Options, kv[0])
		case "tmpfs-mode":
			if len(kv) == 1 {
				return newMount, errors.Wrapf(errBadOptionArg, kv[0])
			}
			newMount.Options = append(newMount.Options, fmt.Sprintf("mode=%s", kv[1]))
		case "tmpfs-size":
			if len(kv) == 1 {
				return newMount, errors.Wrapf(errBadOptionArg, kv[0])
			}
			newMount.Options = append(newMount.Options, fmt.Sprintf("size=%s", kv[1]))
		case "src", "source":
			return newMount, errors.Errorf("source is not supported with tmpfs mounts")
		case "target", "dst", "destination":
			if len(kv) == 1 {
				return newMount, errors.Wrapf(errBadOptionArg, kv[0])
			}
			if err := parse.ValidateVolumeCtrDir(kv[1]); err != nil {
				return newMount, err
			}
			newMount.Destination = kv[1]
			setDest = true
		default:
			return newMount, errors.Wrapf(errBadMntOption, kv[0])
		}
	}

	if !setDest {
		return newMount, errBadVolDest
	}

	return newMount, nil
}

/* This is internal function and could be changed at any time */
/* for external usage please refer to buildah/pkg/parse.GetTempDir() */
func getTempDir() string {
	if tmpdir, ok := os.LookupEnv("TMPDIR"); ok {
		return tmpdir
	}
	return "/var/tmp"
}

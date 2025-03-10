package abi

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/containers/common/libimage"
	"github.com/containers/common/pkg/config"
	"github.com/containers/image/v5/docker"
	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/pkg/compression"
	"github.com/containers/image/v5/signature"
	"github.com/containers/image/v5/transports"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/podman/v4/libpod/define"
	"github.com/containers/podman/v4/pkg/domain/entities"
	"github.com/containers/podman/v4/pkg/domain/entities/reports"
	domainUtils "github.com/containers/podman/v4/pkg/domain/utils"
	"github.com/containers/podman/v4/pkg/errorhandling"
	"github.com/containers/podman/v4/pkg/rootless"
	"github.com/containers/podman/v4/utils"
	"github.com/containers/storage"
	dockerRef "github.com/docker/distribution/reference"
	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

func (ir *ImageEngine) Exists(_ context.Context, nameOrID string) (*entities.BoolReport, error) {
	exists, err := ir.Libpod.LibimageRuntime().Exists(nameOrID)
	if err != nil {
		return nil, err
	}
	return &entities.BoolReport{Value: exists}, nil
}

func (ir *ImageEngine) Prune(ctx context.Context, opts entities.ImagePruneOptions) ([]*reports.PruneReport, error) {
	pruneOptions := &libimage.RemoveImagesOptions{
		RemoveContainerFunc:     ir.Libpod.RemoveContainersForImageCallback(ctx),
		IsExternalContainerFunc: ir.Libpod.IsExternalContainerCallback(ctx),
		ExternalContainers:      opts.External,
		Filters:                 append(opts.Filter, "readonly=false"),
		WithSize:                true,
	}

	if !opts.All {
		pruneOptions.Filters = append(pruneOptions.Filters, "dangling=true")
	}
	if opts.External {
		pruneOptions.Filters = append(pruneOptions.Filters, "containers=external")
	} else {
		pruneOptions.Filters = append(pruneOptions.Filters, "containers=false")
	}

	pruneReports := make([]*reports.PruneReport, 0)

	// Now prune all images until we converge.
	numPreviouslyRemovedImages := 1
	for {
		removedImages, rmErrors := ir.Libpod.LibimageRuntime().RemoveImages(ctx, nil, pruneOptions)
		if rmErrors != nil {
			return nil, errorhandling.JoinErrors(rmErrors)
		}

		for _, rmReport := range removedImages {
			r := *rmReport
			pruneReports = append(pruneReports, &reports.PruneReport{
				Id:   r.ID,
				Size: uint64(r.Size),
			})
		}

		numRemovedImages := len(removedImages)
		if numRemovedImages+numPreviouslyRemovedImages == 0 {
			break
		}
		numPreviouslyRemovedImages = numRemovedImages
	}

	return pruneReports, nil
}

func toDomainHistoryLayer(layer *libimage.ImageHistory) entities.ImageHistoryLayer {
	l := entities.ImageHistoryLayer{}
	l.ID = layer.ID
	l.Created = *layer.Created
	l.CreatedBy = layer.CreatedBy
	copy(l.Tags, layer.Tags)
	l.Size = layer.Size
	l.Comment = layer.Comment
	return l
}

func (ir *ImageEngine) History(ctx context.Context, nameOrID string, opts entities.ImageHistoryOptions) (*entities.ImageHistoryReport, error) {
	image, _, err := ir.Libpod.LibimageRuntime().LookupImage(nameOrID, nil)
	if err != nil {
		return nil, err
	}

	results, err := image.History(ctx)
	if err != nil {
		return nil, err
	}

	history := entities.ImageHistoryReport{
		Layers: make([]entities.ImageHistoryLayer, len(results)),
	}

	for i := range results {
		history.Layers[i] = toDomainHistoryLayer(&results[i])
	}
	return &history, nil
}

func (ir *ImageEngine) Mount(ctx context.Context, nameOrIDs []string, opts entities.ImageMountOptions) ([]*entities.ImageMountReport, error) {
	if opts.All && len(nameOrIDs) > 0 {
		return nil, errors.Errorf("cannot mix --all with images")
	}

	if os.Geteuid() != 0 {
		if driver := ir.Libpod.StorageConfig().GraphDriverName; driver != "vfs" {
			// Do not allow to mount a graphdriver that is not vfs if we are creating the userns as part
			// of the mount command.
			return nil, errors.Errorf("cannot mount using driver %s in rootless mode", driver)
		}

		became, ret, err := rootless.BecomeRootInUserNS("")
		if err != nil {
			return nil, err
		}
		if became {
			os.Exit(ret)
		}
	}

	listImagesOptions := &libimage.ListImagesOptions{}
	if opts.All {
		listImagesOptions.Filters = []string{"readonly=false"}
	}
	images, err := ir.Libpod.LibimageRuntime().ListImages(ctx, nameOrIDs, listImagesOptions)
	if err != nil {
		return nil, err
	}

	mountReports := []*entities.ImageMountReport{}
	listMountsOnly := !opts.All && len(nameOrIDs) == 0
	for _, i := range images {
		// TODO: the .Err fields are not used. This pre-dates the
		// libimage migration but should be addressed at some point.
		// A quick glimpse at cmd/podman/image/mount.go suggests that
		// the errors needed to be handled there as well.
		var mountPoint string
		var err error
		if listMountsOnly {
			// We're only looking for mounted images.
			mountPoint, err = i.Mountpoint()
			if err != nil {
				return nil, err
			}
			// Not mounted, so skip.
			if mountPoint == "" {
				continue
			}
		} else {
			mountPoint, err = i.Mount(ctx, nil, "")
			if err != nil {
				return nil, err
			}
		}

		tags, err := i.RepoTags()
		if err != nil {
			return nil, err
		}
		mountReports = append(mountReports, &entities.ImageMountReport{
			Id:           i.ID(),
			Name:         string(i.Digest()),
			Repositories: tags,
			Path:         mountPoint,
		})
	}
	return mountReports, nil
}

func (ir *ImageEngine) Unmount(ctx context.Context, nameOrIDs []string, options entities.ImageUnmountOptions) ([]*entities.ImageUnmountReport, error) {
	if options.All && len(nameOrIDs) > 0 {
		return nil, errors.Errorf("cannot mix --all with images")
	}

	listImagesOptions := &libimage.ListImagesOptions{}
	if options.All {
		listImagesOptions.Filters = []string{"readonly=false"}
	}
	images, err := ir.Libpod.LibimageRuntime().ListImages(ctx, nameOrIDs, listImagesOptions)
	if err != nil {
		return nil, err
	}

	unmountReports := []*entities.ImageUnmountReport{}
	for _, image := range images {
		r := &entities.ImageUnmountReport{Id: image.ID()}
		mountPoint, err := image.Mountpoint()
		if err != nil {
			r.Err = err
			unmountReports = append(unmountReports, r)
			continue
		}
		if mountPoint == "" {
			// Skip if the image wasn't mounted.
			continue
		}
		r.Err = image.Unmount(options.Force)
		unmountReports = append(unmountReports, r)
	}
	return unmountReports, nil
}

func (ir *ImageEngine) Pull(ctx context.Context, rawImage string, options entities.ImagePullOptions) (*entities.ImagePullReport, error) {
	pullOptions := &libimage.PullOptions{AllTags: options.AllTags}
	pullOptions.AuthFilePath = options.Authfile
	pullOptions.CertDirPath = options.CertDir
	pullOptions.Username = options.Username
	pullOptions.Password = options.Password
	pullOptions.Architecture = options.Arch
	pullOptions.OS = options.OS
	pullOptions.Variant = options.Variant
	pullOptions.SignaturePolicyPath = options.SignaturePolicy
	pullOptions.InsecureSkipTLSVerify = options.SkipTLSVerify

	if !options.Quiet {
		pullOptions.Writer = os.Stderr
	}

	pulledImages, err := ir.Libpod.LibimageRuntime().Pull(ctx, rawImage, options.PullPolicy, pullOptions)
	if err != nil {
		return nil, err
	}

	pulledIDs := make([]string, len(pulledImages))
	for i := range pulledImages {
		pulledIDs[i] = pulledImages[i].ID()
	}

	return &entities.ImagePullReport{Images: pulledIDs}, nil
}

func (ir *ImageEngine) Inspect(ctx context.Context, namesOrIDs []string, opts entities.InspectOptions) ([]*entities.ImageInspectReport, []error, error) {
	reports := []*entities.ImageInspectReport{}
	errs := []error{}

	inspectOptions := &libimage.InspectOptions{WithParent: true, WithSize: true}
	for _, i := range namesOrIDs {
		img, _, err := ir.Libpod.LibimageRuntime().LookupImage(i, nil)
		if err != nil {
			// This is probably a no such image, treat as nonfatal.
			errs = append(errs, err)
			continue
		}
		result, err := img.Inspect(ctx, inspectOptions)
		if err != nil {
			// This is more likely to be fatal.
			return nil, nil, err
		}
		report := entities.ImageInspectReport{}
		if err := domainUtils.DeepCopy(&report, result); err != nil {
			return nil, nil, err
		}
		reports = append(reports, &report)
	}
	return reports, errs, nil
}

func (ir *ImageEngine) Push(ctx context.Context, source string, destination string, options entities.ImagePushOptions) error {
	var manifestType string
	switch options.Format {
	case "":
		// Default
	case "oci":
		manifestType = imgspecv1.MediaTypeImageManifest
	case "v2s1":
		manifestType = manifest.DockerV2Schema1SignedMediaType
	case "v2s2", "docker":
		manifestType = manifest.DockerV2Schema2MediaType
	default:
		return errors.Errorf("unknown format %q. Choose on of the supported formats: 'oci', 'v2s1', or 'v2s2'", options.Format)
	}

	pushOptions := &libimage.PushOptions{}
	pushOptions.AuthFilePath = options.Authfile
	pushOptions.CertDirPath = options.CertDir
	pushOptions.DirForceCompress = options.Compress
	pushOptions.Username = options.Username
	pushOptions.Password = options.Password
	pushOptions.ManifestMIMEType = manifestType
	pushOptions.RemoveSignatures = options.RemoveSignatures
	pushOptions.SignBy = options.SignBy
	pushOptions.InsecureSkipTLSVerify = options.SkipTLSVerify

	compressionFormat := options.CompressionFormat
	if compressionFormat == "" {
		config, err := ir.Libpod.GetConfigNoCopy()
		if err != nil {
			return err
		}
		compressionFormat = config.Engine.CompressionFormat
	}
	if compressionFormat != "" {
		algo, err := compression.AlgorithmByName(compressionFormat)
		if err != nil {
			return err
		}
		pushOptions.CompressionFormat = &algo
	}

	if !options.Quiet {
		pushOptions.Writer = os.Stderr
	}

	pushedManifestBytes, pushError := ir.Libpod.LibimageRuntime().Push(ctx, source, destination, pushOptions)
	if pushError == nil {
		if options.DigestFile != "" {
			manifestDigest, err := manifest.Digest(pushedManifestBytes)
			if err != nil {
				return err
			}

			if err := ioutil.WriteFile(options.DigestFile, []byte(manifestDigest.String()), 0644); err != nil {
				return err
			}
		}
		return nil
	}
	// If the image could not be found, we may be referring to a manifest
	// list but could not find a matching image instance in the local
	// containers storage. In that case, fall back and attempt to push the
	// (entire) manifest.
	if _, err := ir.Libpod.LibimageRuntime().LookupManifestList(source); err == nil {
		_, err := ir.ManifestPush(ctx, source, destination, options)
		return err
	}
	return pushError
}

// Transfer moves images between root and rootless storage so the user specified in the scp call can access and use the image modified by root
func (ir *ImageEngine) Transfer(ctx context.Context, source entities.ImageScpOptions, dest entities.ImageScpOptions, parentFlags []string) error {
	if source.User == "" {
		return errors.Wrapf(define.ErrInvalidArg, "you must define a user when transferring from root to rootless storage")
	}
	podman, err := os.Executable()
	if err != nil {
		return err
	}
	if rootless.IsRootless() && (len(dest.User) == 0 || dest.User == "root") { // if we are rootless and do not have a destination user we can just use sudo
		return transferRootless(source, dest, podman, parentFlags)
	}
	return transferRootful(source, dest, podman, parentFlags)
}

func (ir *ImageEngine) Tag(ctx context.Context, nameOrID string, tags []string, options entities.ImageTagOptions) error {
	// Allow tagging manifest list instead of resolving instances from manifest
	lookupOptions := &libimage.LookupImageOptions{ManifestList: true}
	image, _, err := ir.Libpod.LibimageRuntime().LookupImage(nameOrID, lookupOptions)
	if err != nil {
		return err
	}
	for _, tag := range tags {
		if err := image.Tag(tag); err != nil {
			return err
		}
	}
	return nil
}

func (ir *ImageEngine) Untag(ctx context.Context, nameOrID string, tags []string, options entities.ImageUntagOptions) error {
	image, _, err := ir.Libpod.LibimageRuntime().LookupImage(nameOrID, nil)
	if err != nil {
		return err
	}
	// If only one arg is provided, all names are to be untagged
	if len(tags) == 0 {
		tags = image.Names()
	}
	for _, tag := range tags {
		if err := image.Untag(tag); err != nil {
			return err
		}
	}
	return nil
}

func (ir *ImageEngine) Load(ctx context.Context, options entities.ImageLoadOptions) (*entities.ImageLoadReport, error) {
	loadOptions := &libimage.LoadOptions{}
	loadOptions.SignaturePolicyPath = options.SignaturePolicy
	if !options.Quiet {
		loadOptions.Writer = os.Stderr
	}

	loadedImages, err := ir.Libpod.LibimageRuntime().Load(ctx, options.Input, loadOptions)
	if err != nil {
		return nil, err
	}
	return &entities.ImageLoadReport{Names: loadedImages}, nil
}

func (ir *ImageEngine) Save(ctx context.Context, nameOrID string, tags []string, options entities.ImageSaveOptions) error {
	saveOptions := &libimage.SaveOptions{}
	saveOptions.DirForceCompress = options.Compress
	saveOptions.OciAcceptUncompressedLayers = options.OciAcceptUncompressedLayers

	// Force signature removal to preserve backwards compat.
	// See https://github.com/containers/podman/pull/11669#issuecomment-925250264
	saveOptions.RemoveSignatures = true

	if !options.Quiet {
		saveOptions.Writer = os.Stderr
	}

	names := []string{nameOrID}
	if options.MultiImageArchive {
		names = append(names, tags...)
	} else {
		saveOptions.AdditionalTags = tags
	}
	return ir.Libpod.LibimageRuntime().Save(ctx, names, options.Format, options.Output, saveOptions)
}

func (ir *ImageEngine) Import(ctx context.Context, options entities.ImageImportOptions) (*entities.ImageImportReport, error) {
	importOptions := &libimage.ImportOptions{}
	importOptions.Changes = options.Changes
	importOptions.CommitMessage = options.Message
	importOptions.Tag = options.Reference
	importOptions.SignaturePolicyPath = options.SignaturePolicy
	importOptions.OS = options.OS
	importOptions.Architecture = options.Architecture

	if !options.Quiet {
		importOptions.Writer = os.Stderr
	}

	imageID, err := ir.Libpod.LibimageRuntime().Import(ctx, options.Source, importOptions)
	if err != nil {
		return nil, err
	}

	return &entities.ImageImportReport{Id: imageID}, nil
}

// Search for images using term and filters
func (ir *ImageEngine) Search(ctx context.Context, term string, opts entities.ImageSearchOptions) ([]entities.ImageSearchReport, error) {
	filter, err := libimage.ParseSearchFilter(opts.Filters)
	if err != nil {
		return nil, err
	}

	searchOptions := &libimage.SearchOptions{
		Authfile:              opts.Authfile,
		Filter:                *filter,
		Limit:                 opts.Limit,
		NoTrunc:               true,
		InsecureSkipTLSVerify: opts.SkipTLSVerify,
		ListTags:              opts.ListTags,
	}

	searchResults, err := ir.Libpod.LibimageRuntime().Search(ctx, term, searchOptions)
	if err != nil {
		return nil, err
	}

	// Convert from image.SearchResults to entities.ImageSearchReport. We don't
	// want to leak any low-level packages into the remote client, which
	// requires converting.
	reports := make([]entities.ImageSearchReport, len(searchResults))
	for i := range searchResults {
		reports[i].Index = searchResults[i].Index
		reports[i].Name = searchResults[i].Name
		reports[i].Description = searchResults[i].Description
		reports[i].Stars = searchResults[i].Stars
		reports[i].Official = searchResults[i].Official
		reports[i].Automated = searchResults[i].Automated
		reports[i].Tag = searchResults[i].Tag
	}

	return reports, nil
}

// Config returns a copy of the configuration used by the runtime
func (ir *ImageEngine) Config(_ context.Context) (*config.Config, error) {
	return ir.Libpod.GetConfig()
}

func (ir *ImageEngine) Build(ctx context.Context, containerFiles []string, opts entities.BuildOptions) (*entities.BuildReport, error) {
	id, _, err := ir.Libpod.Build(ctx, opts.BuildOptions, containerFiles...)
	if err != nil {
		return nil, err
	}
	return &entities.BuildReport{ID: id}, nil
}

func (ir *ImageEngine) Tree(ctx context.Context, nameOrID string, opts entities.ImageTreeOptions) (*entities.ImageTreeReport, error) {
	image, _, err := ir.Libpod.LibimageRuntime().LookupImage(nameOrID, nil)
	if err != nil {
		return nil, err
	}
	tree, err := image.Tree(opts.WhatRequires)
	if err != nil {
		return nil, err
	}
	return &entities.ImageTreeReport{Tree: tree}, nil
}

// removeErrorsToExitCode returns an exit code for the specified slice of
// image-removal errors. The error codes are set according to the documented
// behaviour in the Podman man pages.
func removeErrorsToExitCode(rmErrors []error) int {
	var (
		// noSuchImageErrors indicates that at least one image was not found.
		noSuchImageErrors bool
		// inUseErrors indicates that at least one image is being used by a
		// container.
		inUseErrors bool
		// otherErrors indicates that at least one error other than the two
		// above occurred.
		otherErrors bool
	)

	if len(rmErrors) == 0 {
		return 0
	}

	for _, e := range rmErrors {
		switch errors.Cause(e) {
		case storage.ErrImageUnknown, storage.ErrLayerUnknown:
			noSuchImageErrors = true
		case storage.ErrImageUsedByContainer:
			inUseErrors = true
		default:
			otherErrors = true
		}
	}

	switch {
	case inUseErrors:
		// One of the specified images has child images or is
		// being used by a container.
		return 2
	case noSuchImageErrors && !(otherErrors || inUseErrors):
		// One of the specified images did not exist, and no other
		// failures.
		return 1
	default:
		return 125
	}
}

// Remove removes one or more images from local storage.
func (ir *ImageEngine) Remove(ctx context.Context, images []string, opts entities.ImageRemoveOptions) (report *entities.ImageRemoveReport, rmErrors []error) {
	report = &entities.ImageRemoveReport{}

	// Set the exit code at very end.
	defer func() {
		report.ExitCode = removeErrorsToExitCode(rmErrors)
	}()

	libimageOptions := &libimage.RemoveImagesOptions{}
	libimageOptions.Filters = []string{"readonly=false"}
	libimageOptions.Force = opts.Force
	libimageOptions.LookupManifest = opts.LookupManifest
	if !opts.All {
		libimageOptions.Filters = append(libimageOptions.Filters, "intermediate=false")
	}
	libimageOptions.RemoveContainerFunc = ir.Libpod.RemoveContainersForImageCallback(ctx)

	libimageReport, libimageErrors := ir.Libpod.LibimageRuntime().RemoveImages(ctx, images, libimageOptions)

	for _, r := range libimageReport {
		if r.Removed {
			report.Deleted = append(report.Deleted, r.ID)
		}
		report.Untagged = append(report.Untagged, r.Untagged...)
	}

	rmErrors = libimageErrors

	return //nolint
}

// Shutdown Libpod engine
func (ir *ImageEngine) Shutdown(_ context.Context) {
	shutdownSync.Do(func() {
		_ = ir.Libpod.Shutdown(false)
	})
}

func (ir *ImageEngine) Sign(ctx context.Context, names []string, options entities.SignOptions) (*entities.SignReport, error) {
	mech, err := signature.NewGPGSigningMechanism()
	if err != nil {
		return nil, errors.Wrap(err, "error initializing GPG")
	}
	defer mech.Close()
	if err := mech.SupportsSigning(); err != nil {
		return nil, errors.Wrap(err, "signing is not supported")
	}
	sc := ir.Libpod.SystemContext()
	sc.DockerCertPath = options.CertDir
	sc.AuthFilePath = options.Authfile

	for _, signimage := range names {
		err = func() error {
			srcRef, err := alltransports.ParseImageName(signimage)
			if err != nil {
				return errors.Wrapf(err, "error parsing image name")
			}
			rawSource, err := srcRef.NewImageSource(ctx, sc)
			if err != nil {
				return errors.Wrapf(err, "error getting image source")
			}
			defer func() {
				if err = rawSource.Close(); err != nil {
					logrus.Errorf("Unable to close %s image source %q", srcRef.DockerReference().Name(), err)
				}
			}()
			topManifestBlob, manifestType, err := rawSource.GetManifest(ctx, nil)
			if err != nil {
				return errors.Wrapf(err, "error getting manifest blob")
			}
			dockerReference := rawSource.Reference().DockerReference()
			if dockerReference == nil {
				return errors.Errorf("cannot determine canonical Docker reference for destination %s", transports.ImageName(rawSource.Reference()))
			}
			var sigStoreDir string
			if options.Directory != "" {
				repo := reference.Path(dockerReference)
				if path.Clean(repo) != repo { // Coverage: This should not be reachable because /./ and /../ components are not valid in docker references
					return errors.Errorf("Unexpected path elements in Docker reference %s for signature storage", dockerReference.String())
				}
				sigStoreDir = filepath.Join(options.Directory, repo)
			} else {
				signatureURL, err := docker.SignatureStorageBaseURL(sc, rawSource.Reference(), true)
				if err != nil {
					return err
				}
				sigStoreDir, err = localPathFromURI(signatureURL)
				if err != nil {
					return err
				}
			}
			manifestDigest, err := manifest.Digest(topManifestBlob)
			if err != nil {
				return err
			}

			if options.All {
				if !manifest.MIMETypeIsMultiImage(manifestType) {
					return errors.Errorf("%s is not a multi-architecture image (manifest type %s)", signimage, manifestType)
				}
				list, err := manifest.ListFromBlob(topManifestBlob, manifestType)
				if err != nil {
					return errors.Wrapf(err, "Error parsing manifest list %q", string(topManifestBlob))
				}
				instanceDigests := list.Instances()
				for _, instanceDigest := range instanceDigests {
					digest := instanceDigest
					man, _, err := rawSource.GetManifest(ctx, &digest)
					if err != nil {
						return err
					}
					if err = putSignature(man, mech, sigStoreDir, instanceDigest, dockerReference, options); err != nil {
						return errors.Wrapf(err, "error storing signature for %s, %v", dockerReference.String(), instanceDigest)
					}
				}
				return nil
			}
			if err = putSignature(topManifestBlob, mech, sigStoreDir, manifestDigest, dockerReference, options); err != nil {
				return errors.Wrapf(err, "error storing signature for %s, %v", dockerReference.String(), manifestDigest)
			}
			return nil
		}()
		if err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func getSigFilename(sigStoreDirPath string) (string, error) {
	sigFileSuffix := 1
	sigFiles, err := ioutil.ReadDir(sigStoreDirPath)
	if err != nil {
		return "", err
	}
	sigFilenames := make(map[string]bool)
	for _, file := range sigFiles {
		sigFilenames[file.Name()] = true
	}
	for {
		sigFilename := "signature-" + strconv.Itoa(sigFileSuffix)
		if _, exists := sigFilenames[sigFilename]; !exists {
			return sigFilename, nil
		}
		sigFileSuffix++
	}
}

func localPathFromURI(url *url.URL) (string, error) {
	if url.Scheme != "file" {
		return "", errors.Errorf("writing to %s is not supported. Use a supported scheme", url.String())
	}
	return url.Path, nil
}

// putSignature creates signature and saves it to the signstore file
func putSignature(manifestBlob []byte, mech signature.SigningMechanism, sigStoreDir string, instanceDigest digest.Digest, dockerReference dockerRef.Reference, options entities.SignOptions) error {
	newSig, err := signature.SignDockerManifest(manifestBlob, dockerReference.String(), mech, options.SignBy)
	if err != nil {
		return err
	}
	signatureDir := fmt.Sprintf("%s@%s=%s", sigStoreDir, instanceDigest.Algorithm(), instanceDigest.Hex())
	if err := os.MkdirAll(signatureDir, 0751); err != nil {
		// The directory is allowed to exist
		if !os.IsExist(err) {
			return err
		}
	}
	sigFilename, err := getSigFilename(signatureDir)
	if err != nil {
		return err
	}
	if err = ioutil.WriteFile(filepath.Join(signatureDir, sigFilename), newSig, 0644); err != nil {
		return err
	}
	return nil
}

// TransferRootless creates new podman processes using exec.Command and sudo, transferring images between the given source and destination users
func transferRootless(source entities.ImageScpOptions, dest entities.ImageScpOptions, podman string, parentFlags []string) error {
	var cmdSave *exec.Cmd
	saveCommand, loadCommand := parentFlags, parentFlags
	saveCommand = append(saveCommand, []string{"save"}...)
	loadCommand = append(loadCommand, []string{"load"}...)
	if source.Quiet {
		saveCommand = append(saveCommand, "-q")
		loadCommand = append(loadCommand, "-q")
	}

	saveCommand = append(saveCommand, []string{"--output", source.File, source.Image}...)

	loadCommand = append(loadCommand, []string{"--input", dest.File}...)

	if source.User == "root" {
		cmdSave = exec.Command("sudo", podman)
	} else {
		cmdSave = exec.Command(podman)
	}
	cmdSave = utils.CreateSCPCommand(cmdSave, saveCommand)
	logrus.Debugf("Executing save command: %q", cmdSave)
	err := cmdSave.Run()
	if err != nil {
		return err
	}

	var cmdLoad *exec.Cmd
	if source.User != "root" {
		cmdLoad = exec.Command("sudo", podman)
	} else {
		cmdLoad = exec.Command(podman)
	}
	cmdLoad = utils.CreateSCPCommand(cmdLoad, loadCommand)
	logrus.Debugf("Executing load command: %q", cmdLoad)
	return cmdLoad.Run()
}

// TransferRootful creates new podman processes using exec.Command and su/machinectl, transferring images between the given source and destination users
func transferRootful(source entities.ImageScpOptions, dest entities.ImageScpOptions, podman string, parentFlags []string) error {
	basicCommand := []string{podman}
	basicCommand = append(basicCommand, parentFlags...)
	saveCommand := append(basicCommand, "save")
	loadCommand := append(basicCommand, "load")
	if source.Quiet {
		saveCommand = append(saveCommand, "-q")
		loadCommand = append(loadCommand, "-q")
	}
	saveCommand = append(saveCommand, []string{"--output", source.File, source.Image}...)
	loadCommand = append(loadCommand, []string{"--input", dest.File}...)
	save := []string{strings.Join(saveCommand, " ")}
	load := []string{strings.Join(loadCommand, " ")}

	// if executing using sudo or transferring between two users, the TransferRootless approach will not work, default to using machinectl or su as necessary.
	// the approach using sudo is preferable and more straightforward. There is no reason for using sudo in these situations
	// since the feature is meant to transfer from root to rootless an vice versa without explicit sudo evocaiton.
	var uSave *user.User
	var uLoad *user.User
	var err error
	source.User = strings.Split(source.User, ":")[0] // split in case provided with uid:gid
	dest.User = strings.Split(dest.User, ":")[0]
	uSave, err = lookupUser(source.User)
	if err != nil {
		return err
	}
	switch {
	case dest.User != "": // if we are given a destination user, check that first
		uLoad, err = lookupUser(dest.User)
		if err != nil {
			return err
		}
	case uSave.Name != "root": // else if we have no destination user, and source is not root that means we should be root
		uLoad, err = user.LookupId("0")
		if err != nil {
			return err
		}
	default: // else if we have no dest user, and source user IS root, we want to be the default user.
		uString := os.Getenv("SUDO_USER")
		if uString == "" {
			return errors.New("$SUDO_USER must be defined to find the default rootless user")
		}
		uLoad, err = user.Lookup(uString)
		if err != nil {
			return err
		}
	}
	machinectl, err := exec.LookPath("machinectl")
	if err != nil {
		logrus.Warn("defaulting to su since machinectl is not available, su will fail if no user session is available")
		err = execSu(uSave, save)
		if err != nil {
			return err
		}
		return execSu(uLoad, load)
	}
	err = execMachine(uSave, saveCommand, machinectl)
	if err != nil {
		return err
	}
	return execMachine(uLoad, loadCommand, machinectl)
}

func lookupUser(u string) (*user.User, error) {
	if u, err := user.LookupId(u); err == nil {
		return u, nil
	}
	return user.Lookup(u)
}

func execSu(execUser *user.User, command []string) error {
	cmd := exec.Command("su", "-l", execUser.Username, "--command")
	cmd = utils.CreateSCPCommand(cmd, command)
	logrus.Debugf("Executing via su: %q", cmd)
	return cmd.Run()
}

func execMachine(execUser *user.User, command []string, machinectl string) error {
	verb := machinectl
	args := []string{"shell", "-q", execUser.Username + "@.host"}
	if execUser.Uid == "0" {
		args = append([]string{verb}, args...)
		verb = "sudo"
	}
	cmd := utils.CreateSCPCommand(exec.Command(verb, args...), command)
	logrus.Debugf("Executing via machinectl: %q", cmd)
	return cmd.Run()
}

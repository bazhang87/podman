package compat

import (
	"net/http"

	"github.com/containers/podman/v4/libpod"
	"github.com/containers/podman/v4/libpod/define"
	"github.com/containers/podman/v4/pkg/api/handlers/utils"
	"github.com/containers/podman/v4/pkg/api/server/idle"
	api "github.com/containers/podman/v4/pkg/api/types"
	"github.com/gorilla/schema"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

func AttachContainer(w http.ResponseWriter, r *http.Request) {
	runtime := r.Context().Value(api.RuntimeKey).(*libpod.Runtime)
	decoder := r.Context().Value(api.DecoderKey).(*schema.Decoder)

	query := struct {
		DetachKeys string `schema:"detachKeys"`
		Logs       bool   `schema:"logs"`
		Stream     bool   `schema:"stream"`
		Stdin      bool   `schema:"stdin"`
		Stdout     bool   `schema:"stdout"`
		Stderr     bool   `schema:"stderr"`
	}{
		Stream: true,
	}
	if err := decoder.Decode(&query, r.URL.Query()); err != nil {
		utils.Error(w, "Error parsing parameters", http.StatusBadRequest, err)
		return
	}

	// Detach keys: explicitly set to "" is very different from unset
	// TODO: Our format for parsing these may be different from Docker.
	var detachKeys *string
	if _, found := r.URL.Query()["detachKeys"]; found {
		detachKeys = &query.DetachKeys
	}

	streams := new(libpod.HTTPAttachStreams)
	streams.Stdout = true
	streams.Stderr = true
	streams.Stdin = true
	useStreams := false
	if _, found := r.URL.Query()["stdin"]; found {
		streams.Stdin = query.Stdin
		useStreams = true
	}
	if _, found := r.URL.Query()["stdout"]; found {
		streams.Stdout = query.Stdout
		useStreams = true
	}
	if _, found := r.URL.Query()["stderr"]; found {
		streams.Stderr = query.Stderr
		useStreams = true
	}
	if !useStreams {
		streams = nil
	}
	if useStreams && !streams.Stdout && !streams.Stderr && !streams.Stdin {
		utils.Error(w, "Parameter conflict", http.StatusBadRequest, errors.Errorf("at least one of stdin, stdout, stderr must be true"))
		return
	}

	// At least one of these must be set
	if !query.Stream && !query.Logs {
		utils.Error(w, "Unsupported parameter", http.StatusBadRequest, errors.Errorf("at least one of Logs or Stream must be set"))
		return
	}

	name := utils.GetName(r)
	ctr, err := runtime.LookupContainer(name)
	if err != nil {
		utils.ContainerNotFound(w, name, err)
		return
	}

	state, err := ctr.State()
	if err != nil {
		utils.InternalServerError(w, err)
		return
	}
	// For Docker compatibility, we need to re-initialize containers in these states.
	if state == define.ContainerStateConfigured || state == define.ContainerStateExited {
		if err := ctr.Init(r.Context(), ctr.PodID() != ""); err != nil {
			utils.Error(w, "Container in wrong state", http.StatusConflict, errors.Wrapf(err, "error preparing container %s for attach", ctr.ID()))
			return
		}
	} else if !(state == define.ContainerStateCreated || state == define.ContainerStateRunning) {
		utils.InternalServerError(w, errors.Wrapf(define.ErrCtrStateInvalid, "can only attach to created or running containers - currently in state %s", state.String()))
		return
	}

	logErr := func(e error) {
		logrus.Error(errors.Wrapf(e, "error attaching to container %s", ctr.ID()))
	}

	// Perform HTTP attach.
	// HTTPAttach will handle everything about the connection from here on
	// (including closing it and writing errors to it).
	hijackChan := make(chan bool, 1)
	err = ctr.HTTPAttach(r, w, streams, detachKeys, nil, query.Stream, query.Logs, hijackChan)

	if <-hijackChan {
		// If connection was Hijacked, we have to signal it's being closed
		t := r.Context().Value(api.IdleTrackerKey).(*idle.Tracker)
		defer t.Close()

		if err != nil {
			// Cannot report error to client as a 500 as the Upgrade set status to 101
			logErr(err)
		}
	} else {
		// If the Hijack failed we are going to assume we can still inform client of failure
		utils.InternalServerError(w, err)
		logErr(err)
	}
	logrus.Debugf("Attach for container %s completed successfully", ctr.ID())
}

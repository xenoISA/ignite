package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net"
	"time"

	refdocker "github.com/containerd/containerd/reference/docker"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	cont "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/client"
	log "github.com/sirupsen/logrus"
	meta "github.com/weaveworks/ignite/pkg/apis/meta/v1alpha1"
	"github.com/weaveworks/ignite/pkg/preflight"
	"github.com/weaveworks/ignite/pkg/preflight/checkers"
	"github.com/weaveworks/ignite/pkg/providers"
	"github.com/weaveworks/ignite/pkg/runtime"
	"github.com/weaveworks/ignite/pkg/runtime/auth"
	"github.com/weaveworks/ignite/pkg/util"
)

const (
	dcSocket = "/var/run/docker.sock"
)

// dockerClient is a runtime.Interface
// implementation serving the Docker client
type dockerClient struct {
	client *client.Client
}

var _ runtime.Interface = &dockerClient{}

// GetDockerClient builds a client for talking to docker
func GetDockerClient() (*dockerClient, error) {
	// Pin the REST API version exactly as upstream v0.10.0 did. The moby
	// client does not validate a fixed version against its negotiation
	// floor, so requests keep going to /v1.35/... on the node daemon.
	cli, err := client.New(client.FromEnv, client.WithAPIVersion("1.35"))
	if err != nil {
		return nil, err
	}

	return &dockerClient{
		client: cli,
	}, nil
}

func (dc *dockerClient) PullImage(image meta.OCIImageRef) (err error) {
	var rc io.ReadCloser

	opts := client.ImagePullOptions{}

	// Get the domain name from the image.
	named, err := refdocker.ParseDockerRef(image.String())
	if err != nil {
		return err
	}
	refDomain := refdocker.Domain(named)
	// Default the host for docker.io.
	refDomain, err = docker.DefaultHost(refDomain)
	if err != nil {
		return err
	}

	// Get available credentials from docker cli config.
	authCreds, _, err := auth.NewAuthCreds(refDomain, providers.RegistryConfigDir)
	if err != nil {
		return err
	}
	if authCreds != nil {
		// Encode the credentials and set it in the pull options.
		authConfig := registry.AuthConfig{}
		authConfig.Username, authConfig.Password, err = authCreds(refDomain)
		if err != nil {
			return err
		}
		encodedJSON, err := json.Marshal(authConfig)
		if err != nil {
			return err
		}
		authStr := base64.URLEncoding.EncodeToString(encodedJSON)
		opts.RegistryAuth = authStr
	}

	if rc, err = dc.client.ImagePull(context.Background(), image.Normalized(), opts); err == nil {
		// Don't output the pull command
		defer util.DeferErr(&err, rc.Close)
		_, err = io.Copy(ioutil.Discard, rc)
	}

	return
}

func (dc *dockerClient) InspectImage(image meta.OCIImageRef) (*runtime.ImageInspectResult, error) {
	res, err := dc.client.ImageInspect(context.Background(), image.Normalized())
	if err != nil {
		return nil, err
	}

	// By default parse the OCI content ID from the Docker image ID
	contentRef := res.ID
	if len(res.RepoDigests) > 0 {
		// If the image has Repo digests, use the first one of them to parse
		// the fully qualified OCI image name and digest. All of the digests
		// point to the same contents, so it doesn't matter which one we use
		// here. It will be translated to the right content by the runtime.
		contentRef = res.RepoDigests[0]
	}

	// Parse the OCI content ID based on the available reference
	id, err := meta.ParseOCIContentID(contentRef)
	if err != nil {
		return nil, err
	}

	r := &runtime.ImageInspectResult{
		ID:   id,
		Size: res.Size,
	}

	return r, nil
}

func (dc *dockerClient) ExportImage(image meta.OCIImageRef) (r io.ReadCloser, cleanup func() error, err error) {
	config, err := dc.client.ContainerCreate(context.Background(), client.ContainerCreateOptions{
		Config: &container.Config{
			Cmd:   []string{"sh"}, // We need a temporary command, this doesn't need to exist in the image
			Image: image.Normalized(),
		},
	})
	if err != nil {
		return
	}

	if r, err = dc.client.ContainerExport(context.Background(), config.ID, client.ContainerExportOptions{}); err == nil {
		cleanup = func() error { return dc.RemoveContainer(config.ID) }
	}

	return
}

func (dc *dockerClient) InspectContainer(container string) (*runtime.ContainerInspectResult, error) {
	inspect, err := dc.client.ContainerInspect(context.Background(), container, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	res := inspect.Container

	var status string
	var pid int
	if res.State != nil {
		status = string(res.State.Status)
		pid = res.State.Pid
	}

	return &runtime.ContainerInspectResult{
		ID:        res.ID,
		Image:     res.Image,
		Status:    status,
		IPAddress: net.ParseIP(legacyNetworkSettingsIPAddress(inspect.Raw)),
		PID:       uint32(pid),
	}, nil
}

func (dc *dockerClient) AttachContainer(container string) (err error) {
	// TODO: Rework to perform the attach via the Docker client,
	// this will require manual TTY and signal emulation/handling.
	// Implement the pseudo-TTY and remove this call, see
	// https://github.com/weaveworks/ignite/pull/211#issuecomment-512809841
	var ec int
	if ec, err = util.ExecForeground("docker", "attach", container); err != nil {
		if ec == 1 { // Docker's detach sequence (^P^Q) has an exit code of -1
			err = nil // Don't treat it as an error
		}
	}

	return
}

func (dc *dockerClient) RunContainer(image meta.OCIImageRef, config *runtime.ContainerConfig, name, id string) (string, error) {
	binds := make([]string, 0, len(config.Binds))
	for _, bind := range config.Binds {
		binds = append(binds, fmt.Sprintf("%s:%s", bind.HostPath, bind.ContainerPath))
	}

	devices := make([]container.DeviceMapping, 0, len(config.Devices))
	for _, device := range config.Devices {
		devices = append(devices, container.DeviceMapping{
			PathOnHost:        device.HostPath,
			PathInContainer:   device.ContainerPath,
			CgroupPermissions: "rwm",
		})
	}

	stopTimeout := int(config.StopTimeout)
	bindings, exposed, err := portBindingsToDocker(config.PortBindings)
	if err != nil {
		return "", err
	}

	c, err := dc.client.ContainerCreate(context.Background(), client.ContainerCreateOptions{
		Config: &container.Config{
			Hostname:     config.Hostname,
			ExposedPorts: exposed,
			Tty:          true, // --tty
			OpenStdin:    true, // --interactive
			Cmd:          config.Cmd,
			Image:        image.Normalized(),
			Labels:       config.Labels,
			Env:          config.EnvVars,
			StopTimeout:  &stopTimeout,
		},
		HostConfig: &container.HostConfig{
			Binds:        binds,
			NetworkMode:  container.NetworkMode(config.NetworkMode),
			PortBindings: bindings,
			AutoRemove:   config.AutoRemove,
			CapAdd:       config.CapAdds,
			Resources: container.Resources{
				Devices: devices,
			},
		},
		Name: name,
	})
	if err != nil {
		return "", err
	}

	_, err = dc.client.ContainerStart(context.Background(), c.ID, client.ContainerStartOptions{})
	return c.ID, err
}

func (dc *dockerClient) StopContainer(container string, timeout *time.Duration) error {
	// Start waiting before we do the stop, to avoid race
	errC, readyC := make(chan error), make(chan struct{})
	go func() {
		errC <- dc.waitForContainer(container, cont.WaitConditionNotRunning, &readyC)
	}()
	<-readyC // wait until removal detection has started

	if _, err := dc.client.ContainerStop(context.Background(), container, client.ContainerStopOptions{Timeout: durationToSeconds(timeout)}); err != nil {
		// If the container is not found, return nil, no-op.
		if errdefs.IsNotFound(err) {
			log.Warn(err)
			return nil
		}
		return err
	}

	// Wait for the container to be stopped
	return <-errC
}

func (dc *dockerClient) KillContainer(container, signal string) error {
	// Start waiting before we do the kill, to avoid race
	errC, readyC := make(chan error), make(chan struct{})
	go func() {
		errC <- dc.waitForContainer(container, cont.WaitConditionNotRunning, &readyC)
	}()
	<-readyC // wait until removal detection has started

	if _, err := dc.client.ContainerKill(context.Background(), container, client.ContainerKillOptions{Signal: signal}); err != nil {
		// If the container is not found, return nil, no-op.
		if errdefs.IsNotFound(err) {
			log.Warn(err)
			return nil
		}
		return err
	}

	// Wait for the container to be killed
	return <-errC
}

func (dc *dockerClient) RemoveContainer(container string) error {
	// Container waiting can fail if the container gets removed before
	// we reach the waiting fence. Start the waiter in a goroutine before
	// performing the removal to ensure proper removal detection.
	errC := make(chan error)
	readyC := make(chan struct{})
	go func() {
		errC <- dc.waitForContainer(container, cont.WaitConditionRemoved, &readyC)
	}()

	<-readyC // The ready channel is used to wait until removal detection has started
	if _, err := dc.client.ContainerRemove(context.Background(), container, client.ContainerRemoveOptions{}); err != nil {
		// If the container is not found, return nil, no-op.
		if errdefs.IsNotFound(err) {
			log.Warn(err)
			return nil
		}
		return err
	}

	// Wait for the container to be removed
	return <-errC
}

func (dc *dockerClient) ContainerLogs(container string) (io.ReadCloser, error) {
	return dc.client.ContainerLogs(context.Background(), container, client.ContainerLogsOptions{
		ShowStdout: true, // We only need stdout, as TTY mode merges stderr into it
	})
}

func (cc *dockerClient) Name() runtime.Name {
	return runtime.RuntimeDocker
}

func (dc *dockerClient) RawClient() interface{} {
	return dc.client
}

func (dc *dockerClient) PreflightChecker() preflight.Checker {
	return checkers.NewExistingFileChecker(dcSocket)
}

func (dc *dockerClient) waitForContainer(container string, condition cont.WaitCondition, readyC *chan struct{}) error {
	wait := dc.client.ContainerWait(context.Background(), container, client.ContainerWaitOptions{Condition: condition})
	resultC, errC := wait.Result, wait.Error

	// The ready channel can be used to wait until
	// the container wait request has been sent to
	// Docker to ensure proper ordering
	if readyC != nil {
		*readyC <- struct{}{}
	}

	select {
	case result := <-resultC:
		if result.Error != nil {
			return fmt.Errorf("failed to wait for container %q: %s", container, result.Error.Message)
		}
	case err := <-errC:
		return fmt.Errorf("error waiting for container %q: %w", container, err)
	}

	return nil
}

// durationToSeconds converts the optional stop timeout into the whole-second
// value the Engine API takes, rounding the same way the v20.10 client did
// (strconv.FormatFloat(d.Seconds(), 'f', 0, 64)).
func durationToSeconds(timeout *time.Duration) *int {
	if timeout == nil {
		return nil
	}
	seconds := int(math.Round(timeout.Seconds()))
	return &seconds
}

// legacyNetworkSettingsIPAddress reads the top-level NetworkSettings.IPAddress
// field (the default-bridge address) from the raw inspect body. The pinned
// API version still returns it, but current moby/moby/api types no longer
// decode it, and ignite's docker-bridge networking depends on it.
func legacyNetworkSettingsIPAddress(raw []byte) string {
	var body struct {
		NetworkSettings *struct {
			IPAddress string `json:"IPAddress"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.NetworkSettings == nil {
		return ""
	}
	return body.NetworkSettings.IPAddress
}

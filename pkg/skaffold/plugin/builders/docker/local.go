/*
Copyright 2019 The Skaffold Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package docker

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"

	configutil "github.com/GoogleContainerTools/skaffold/cmd/skaffold/app/cmd/config"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/build"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/build/tag"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/color"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/docker"
	kubectx "github.com/GoogleContainerTools/skaffold/pkg/skaffold/kubernetes/context"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/schema/latest"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/util"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/warnings"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

func (b *Builder) local(ctx context.Context, out io.Writer, tags tag.ImageTags, artifacts []*latest.Artifact) ([]build.Artifact, error) {
	var l *latest.LocalBuild
	if err := util.CloneThroughJSON(b.env.Properties, &l); err != nil {
		return nil, errors.Wrap(err, "converting execution env to localBuild struct")
	}
	if l == nil {
		l = &latest.LocalBuild{}
	}
	b.LocalBuild = l
	kubeContext, err := kubectx.CurrentContext()
	if err != nil {
		return nil, errors.Wrap(err, "getting current cluster context")
	}
	b.KubeContext = kubeContext
	localDocker, err := docker.NewAPIClient(b.opts.Prune(), b.insecureRegistries)
	if err != nil {
		return nil, errors.Wrap(err, "getting docker client")
	}
	b.LocalDocker = localDocker
	localCluster, err := configutil.GetLocalCluster()
	if err != nil {
		return nil, errors.Wrap(err, "getting localCluster")
	}
	b.LocalCluster = localCluster
	var pushImages bool
	if b.LocalBuild.Push == nil {
		pushImages = !localCluster
		logrus.Debugf("push value not present, defaulting to %t because localCluster is %t", pushImages, localCluster)
	} else {
		pushImages = *b.LocalBuild.Push
	}
	b.PushImages = pushImages
	for _, a := range artifacts {
		if err := setArtifact(a); err != nil {
			return nil, err
		}
	}
	return b.buildArtifacts(ctx, out, tags, artifacts)
}

func (b *Builder) prune(ctx context.Context, out io.Writer) error {
	return docker.Prune(ctx, out, b.builtImages, b.LocalDocker)
}

func (b *Builder) buildArtifacts(ctx context.Context, out io.Writer, tags tag.ImageTags, artifacts []*latest.Artifact) ([]build.Artifact, error) {
	if b.LocalCluster {
		color.Default.Fprintf(out, "Found [%s] context, using local docker daemon.\n", b.KubeContext)
	}
	return build.InSequence(ctx, out, tags, artifacts, b.runBuild)
}

func (b *Builder) runBuild(ctx context.Context, out io.Writer, artifact *latest.Artifact, tag string) (string, error) {
	digestOrImageID, err := b.BuildArtifact(ctx, out, artifact, tag)
	if err != nil {
		return "", errors.Wrap(err, "build artifact")
	}
	if b.PushImages {
		imageID, err := b.getImageIDForTag(ctx, tag)
		if err != nil {
			logrus.Warnf("unable to inspect image: built images may not be cleaned up correctly by skaffold")
		}
		b.builtImages = append(b.builtImages, imageID)
		digest := digestOrImageID
		return tag + "@" + digest, nil
	}

	// k8s doesn't recognize the imageID or any combination of the image name
	// suffixed with the imageID, as a valid image name.
	// So, the solution we chose is to create a tag, just for Skaffold, from
	// the imageID, and use that in the manifests.
	imageID := digestOrImageID
	b.builtImages = append(b.builtImages, imageID)
	uniqueTag := artifact.ImageName + ":" + strings.TrimPrefix(imageID, "sha256:")
	if err := b.LocalDocker.Tag(ctx, imageID, uniqueTag); err != nil {
		return "", err
	}

	return uniqueTag, nil
}

// BuildArtifact builds the docker artifact
func (b *Builder) BuildArtifact(ctx context.Context, out io.Writer, a *latest.Artifact, tag string) (string, error) {
	if err := b.pullCacheFromImages(ctx, out, a.ArtifactType.DockerArtifact); err != nil {
		return "", errors.Wrap(err, "pulling cache-from images")
	}

	var (
		imageID string
		err     error
	)

	if b.LocalBuild.UseDockerCLI || b.LocalBuild.UseBuildkit {
		imageID, err = b.dockerCLIBuild(ctx, out, a.Workspace, a.ArtifactType.DockerArtifact, tag)
	} else {
		imageID, err = b.LocalDocker.Build(ctx, out, a.Workspace, a.ArtifactType.DockerArtifact, tag)
	}

	if err != nil {
		return "", err
	}

	if b.PushImages {
		return b.LocalDocker.Push(ctx, out, tag)
	}

	return imageID, nil
}

func (b *Builder) dockerCLIBuild(ctx context.Context, out io.Writer, workspace string, a *latest.DockerArtifact, tag string) (string, error) {
	dockerfilePath, err := docker.NormalizeDockerfilePath(workspace, a.DockerfilePath)
	if err != nil {
		return "", errors.Wrap(err, "normalizing dockerfile path")
	}

	args := []string{"build", workspace, "--file", dockerfilePath, "-t", tag}
	ba, err := docker.GetBuildArgs(a)
	if err != nil {
		return "", err
	}
	args = append(args, ba...)
	if b.opts.Prune() {
		args = append(args, "--force-rm")
	}

	cmd := exec.CommandContext(ctx, "docker", args...)
	if b.LocalBuild.UseBuildkit {
		cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	}
	cmd.Stdout = out
	cmd.Stderr = out

	if err := util.RunCmd(cmd); err != nil {
		return "", errors.Wrap(err, "running build")
	}

	return b.LocalDocker.ImageID(ctx, tag)
}

func (b *Builder) pullCacheFromImages(ctx context.Context, out io.Writer, a *latest.DockerArtifact) error {
	if len(a.CacheFrom) == 0 {
		return nil
	}

	for _, image := range a.CacheFrom {
		imageID, err := b.LocalDocker.ImageID(ctx, image)
		if err != nil {
			return errors.Wrapf(err, "getting imageID for %s", image)
		}
		if imageID != "" {
			// already pulled
			continue
		}

		if err := b.LocalDocker.Pull(ctx, out, image); err != nil {
			warnings.Printf("Cache-From image couldn't be pulled: %s\n", image)
		}
	}

	return nil
}

func (b *Builder) getImageIDForTag(ctx context.Context, tag string) (string, error) {
	insp, _, err := b.LocalDocker.ImageInspectWithRaw(ctx, tag)
	if err != nil {
		return "", errors.Wrap(err, "inspecting image")
	}
	return insp.ID, nil
}

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/imageprovider"
	"github.com/openshift/hypershift/support/releaseinfo"
	"github.com/openshift/hypershift/support/releaseinfo/registryclient"
)

var pullSecret = []byte(``)

func main() {
	ctx := context.Background()

	registryProvider := &releaseinfo.RegistryClientProvider{}
	releaseImage := "registry.build05.ci.openshift.org/ci-ln-clnj9zk/release:latest"
	// Look up the release image metadata
	imageProvider, err := func() (*imageprovider.ReleaseImageProvider, error) {
		img, err := registryProvider.Lookup(ctx, releaseImage, pullSecret)
		if err != nil {
			return nil, fmt.Errorf("failed to look up release image metadata: %w", err)
		}
		return imageprovider.New(img), nil
	}()
	if err != nil {
		panic(fmt.Errorf("failed to get component images: %v", err))
	}
	installerImage, ok := imageProvider.ImageExist("installer")
	if !ok {
		panic(fmt.Errorf("failed to find installer image"))
	}
	installerImage, err = registryclient.GetCorrectArchImage(ctx, "installer", installerImage, pullSecret)
	if err != nil {
		panic(fmt.Errorf("failed to get correct arch image: %v", err))
	}

	workDir, err := os.MkdirTemp("", "openshift-install")
	if err != nil {
		panic(fmt.Errorf("failed to create working directory: %w", err))
	}
	binDir := filepath.Join(workDir, "bin")
	for _, dir := range []string{binDir} {
		if err := os.Mkdir(dir, 0755); err != nil {
			panic(fmt.Errorf("failed to make directory %s: %w", dir, err))
		}
	}

	if err := registryclient.ExtractImageFilesToDir(ctx, installerImage, pullSecret, "openshift-install", binDir); err != nil {
		panic(fmt.Errorf("failed to extract openshift-install binary: %w", err))
	}

	fmt.Printf("extracted openshift-install in binDir: %q\n", binDir)
}

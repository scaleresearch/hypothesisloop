package domain

import "strings"

// JobImage is an image the deployment offers a job to run in, so that an agent need not build
// a framework into a bare one. A job names one by Name or states the whole address of another
// image (ResolveJobImage).
type JobImage struct {
	Name    string `json:"name" doc:"What the image is called here, which job.image takes."`
	Image   string `json:"image" doc:"Its whole address, which job.image takes as well and a job that named it is stored with."`
	Carries string `json:"carries" doc:"What is installed in it."`
	// Suits is prefixes of accelerator type names. None means every type.
	Suits []string `json:"suits,omitempty" doc:"The accelerator types it suits, as prefixes of their names. Absent: every type."`
}

// SuitsType says the image is meant for jobs on acceleratorType.
func (i JobImage) SuitsType(acceleratorType string) bool {
	for _, prefix := range i.Suits {
		if strings.HasPrefix(acceleratorType, prefix) {
			return true
		}
	}
	return len(i.Suits) == 0
}

// ResolveJobImage is the address of the image a job stating image runs in: that of the offered
// image it names, or image itself when it is a whole registry address. Anything else names no
// image the platform could pull as the job meant it, and is no image: a bare name such as
// "python" would be pulled from whatever registry the runtime defaults to.
func ResolveJobImage(offered []JobImage, image string) (string, bool) {
	for _, o := range offered {
		if o.Name == image {
			return o.Image, true
		}
	}
	return image, isRegistryAddress(image)
}

// isRegistryAddress reports whether ref starts with the host of a registry, the way a container
// engine tells one from a path on its default registry: a first segment with a dot or a port, or
// localhost.
func isRegistryAddress(ref string) bool {
	host, path, found := strings.Cut(ref, "/")
	if !found || path == "" || strings.ContainsFunc(ref, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return false
	}
	return host == "localhost" || strings.ContainsAny(host, ".:")
}

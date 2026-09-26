package domain

import "strings"

// JobImage is an image the deployment offers a job to run in, so that an agent need not build
// a framework into a bare one. A job may name any other image: the list is an offer.
type JobImage struct {
	Name    string `json:"name" doc:"What the image is called here."`
	Image   string `json:"image" doc:"The reference job.image takes."`
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

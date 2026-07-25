package main

import (
	"net/http"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

type agent struct {
	clusterName string
	cpURL       string
	jwc         *workload.JobWorkloadClient
	httpClient  *http.Client
}

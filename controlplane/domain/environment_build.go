package domain

import "github.com/scaleresearch/hypothesisloop/contract/clusterapi"

// The build vocabulary is the cluster contract's own (clusterapi).
type (
	EnvironmentBuild                  = clusterapi.EnvironmentBuild
	RegistryLogin                     = clusterapi.RegistryLogin
	EnvironmentBuildCredentialRequest = clusterapi.EnvironmentBuildCredentialRequest
	EnvironmentGitCredential          = clusterapi.EnvironmentGitCredential
	EnvironmentBuildOutcomeKind       = clusterapi.EnvironmentBuildOutcomeKind
	EnvironmentBuildReport            = clusterapi.EnvironmentBuildReport
	EnvironmentBuildLoaded            = clusterapi.EnvironmentBuildLoaded
	EnvironmentBuildProgress          = clusterapi.EnvironmentBuildProgress
)

const (
	EnvironmentBuildPushed        = clusterapi.EnvironmentBuildPushed
	EnvironmentBuildFileFault     = clusterapi.EnvironmentBuildFileFault
	EnvironmentBuildPlatformFault = clusterapi.EnvironmentBuildPlatformFault
	MaxEnvironmentProgressBytes   = clusterapi.MaxEnvironmentProgressBytes
)

// MaxNodeNameBytes bounds a node's name, a DNS subdomain.
const MaxNodeNameBytes = 253

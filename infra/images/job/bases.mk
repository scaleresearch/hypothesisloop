# The job images that carry a framework (job_images in the settings file): the upstream image
# with the job image's own tools added. The root Makefile and infra/Makefile both include this.
# An entry is name=base, or name=base=version for a base that gets CPU PyTorch at that version.
# The tt-metal release ships no torch: 2.11.0 is what its v0.77.0 pins for x86_64 development.
JOB_IMAGE_BASES := job-pytorch-cuda=docker.io/pytorch/pytorch@sha256:68c022c2f4627943a6f3e574cfd2c8ae4256210d5f66ae2b117942e0a8d4fa9d \
	job-tenstorrent=ghcr.io/tenstorrent/tt-metal/tt-metalium-ubuntu-22.04-release-amd64:v0.77.0=2.11.0

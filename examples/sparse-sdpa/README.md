# sparse-sdpa

Kernel research on Tenstorrent Blackhole: minimize the device kernel duration of
`ttnn.transformer.sparse_sdpa` on the upstream "prod-sparse" shape. It ranks `kernel_duration_ns`
and gates on `pcc_achieved` of at least 0.99 against the Torch golden. Researchers vary the
harness's env vars or patch tt-metal itself (`build_and_run.sh`).

Needs a cluster with `tenstorrent.com/chipArch=blackhole`, the `tenstorrent` job image, and access
to github.com from jobs. Start it as [../README.md](../README.md) says.

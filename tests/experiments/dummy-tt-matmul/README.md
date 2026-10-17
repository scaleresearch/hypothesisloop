# dummy-tt-matmul

A Tenstorrent smoke test of the whole agent flow: a bf16 matmul benchmark on one Blackhole ASIC.
It ranks `tflops_measured`, the best sustained throughput, and reports `latency_ms` beside it.

Needs a cluster with `tenstorrent.com/chipArch=blackhole` and the `tenstorrent` job image. Start
it as [../README.md](../README.md) says.

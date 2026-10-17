# shampoo-family-optimizers

Optimizer research on CPU: on a fixed MLP autoencoder over Fashion-MNIST, how much the Shampoo
family of second-order optimizers beats AdamW, and whether a new variant beats them all. It ranks
`optimizer_flops_to_target`, the analytic FLOPs to reach a fixed test MSE, and gates on `diverged`.

Runs on any stack with CPU cores and access to PyPI and download.pytorch.org from jobs. Start it
as [../README.md](../README.md) says.

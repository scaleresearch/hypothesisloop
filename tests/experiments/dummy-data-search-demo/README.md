# dummy-data-search-demo

A CPU smoke test of the whole agent flow: a numpy MLP on a synthetic two-class dataset, a trial
in about a minute on one core. It ranks `val_accuracy` and reports `train_loss` beside it.
Executors vary job env vars along three axes: architecture, hyperparameters and data.

Runs on any stack with CPU cores. Start it as [../README.md](../README.md) says.

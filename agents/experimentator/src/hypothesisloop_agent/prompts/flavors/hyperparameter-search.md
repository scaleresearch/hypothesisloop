Flavor: hyperparameter-search. Hold architecture and data fixed. Find the setting of learning
rate, batch size, regularization and optimizer knobs that wins.

Your `hyperparameters` are a starting point, not a fixed assignment. Read what earlier trials
found, then propose the next point deliberately: refine under the best point so far, registered
as a hypothesis of its own, and repeat no point already tried. Name the point you're testing and
why you expect it to move the metric.

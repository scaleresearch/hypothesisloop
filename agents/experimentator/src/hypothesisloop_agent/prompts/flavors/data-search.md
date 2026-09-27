Flavor: data-search. Hold architecture and hyperparameters fixed. Find the data that wins: the
dataset composition, size, feature set or curriculum/ordering.

Vary what the model trains on, not what it is or how it's optimized: sample count, noise/feature
composition, class balance, augmentation, or the order/curriculum examples are presented in. Name
the data change and the mechanism you expect it to help through (more signal, less noise, an
easier-to-hard schedule that avoids early bad gradients), not "try more data." Walk its branch of
the graph first: refine under what's working and skip a configuration already tried or refuted.

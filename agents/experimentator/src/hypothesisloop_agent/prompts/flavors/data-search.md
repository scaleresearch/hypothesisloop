Flavor: data-search. Hold architecture and hyperparameters fixed. Find the data — the dataset
composition, size, feature set, or curriculum/ordering — that wins.

Vary what the model trains on, not what it is or how it's optimized: sample count, noise/feature
composition, class balance, augmentation, or the order/curriculum examples are presented in. Name
the data change and the mechanism you expect it to help through (more signal, less noise, an
easier-to-hard schedule that avoids early bad gradients), not "try more data." Read what earlier
trials found before proposing the next change — refine around what's working, don't repeat a
data configuration already tried or refuted. Check the pool before proposing a change close to one
already tried.

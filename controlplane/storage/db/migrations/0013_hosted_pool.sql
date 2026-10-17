-- The launcher's warm pool as it last reported it, held so that a pool machine its provider's
-- list leaves out is read by its id and not warmed again.
CREATE TABLE hosted_pool (
    machine TEXT PRIMARY KEY,
    name    TEXT NOT NULL
);

-- The word flavor is gone: an eviction for landing on an accelerator type other than the one
-- reserved is accelerator_type_mismatch, wherever the reason was kept.
UPDATE experiments SET eviction_reason = regexp_replace(eviction_reason, '^flavor_mismatch', 'accelerator_type_mismatch')
WHERE eviction_reason ~ '^flavor_mismatch(:|$)';
UPDATE experiments SET infra_requeue_reason = regexp_replace(infra_requeue_reason, '^flavor_mismatch', 'accelerator_type_mismatch')
WHERE infra_requeue_reason ~ '^flavor_mismatch(:|$)';
UPDATE experiments SET tried_clusters = (
    SELECT jsonb_agg(CASE WHEN t.entry->>'reason' = 'flavor_mismatch'
                          THEN jsonb_set(t.entry, '{reason}', '"accelerator_type_mismatch"') ELSE t.entry END
                     ORDER BY t.n)
    FROM jsonb_array_elements(tried_clusters) WITH ORDINALITY AS t(entry, n))
WHERE tried_clusters @> '[{"reason": "flavor_mismatch"}]';
UPDATE changes SET detail = regexp_replace(detail, '^flavor_mismatch', 'accelerator_type_mismatch')
WHERE kind = 'experiment.status' AND detail ~ '^flavor_mismatch(:|$)';

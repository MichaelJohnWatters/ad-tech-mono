-- +goose Up

-- delivery=webhook: the report-runner announces completed jobs on
-- adtech.report.completed and the webhooks dispatcher delivers them to the
-- account's subscriptions. The report_jobs CHECK predates the mode and
-- rejected the value at INSERT (the gateway 500 the webhook-delivery e2e
-- caught).
ALTER TABLE report_jobs DROP CONSTRAINT report_jobs_delivery_check;
ALTER TABLE report_jobs ADD CONSTRAINT report_jobs_delivery_check
    CHECK (delivery IN ('email', 'webhook', 'none'));

-- +goose Down
ALTER TABLE report_jobs DROP CONSTRAINT report_jobs_delivery_check;
ALTER TABLE report_jobs ADD CONSTRAINT report_jobs_delivery_check
    CHECK (delivery IN ('email', 'none'));

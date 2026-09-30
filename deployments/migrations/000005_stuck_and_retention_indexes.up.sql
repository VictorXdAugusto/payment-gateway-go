-- Reconciliação: só pagamentos presos (created/unknown) entram no índice. A varredura periódica
-- custa proporcional ao que está preso, não ao total de pagamentos.
CREATE INDEX payments_stuck_idx ON payments (id) WHERE status IN ('created', 'unknown');

-- Retenção da outbox: eventos já entregues ou dispensados podem ser apagados depois de um tempo.
-- Os dead ficam para inspeção e reenvio manual, por isso não entram no índice.
CREATE INDEX outbox_retention_idx ON outbox_events (created_at) WHERE status IN ('delivered', 'skipped');

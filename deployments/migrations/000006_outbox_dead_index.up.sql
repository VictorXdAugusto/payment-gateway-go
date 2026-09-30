-- O gauge de eventos mortos (alerta: há entrega que desistiu) consulta a cada scrape.
-- Dead é raro, então o índice parcial é minúsculo e a contagem é praticamente de graça.
CREATE INDEX outbox_dead_idx ON outbox_events (id) WHERE status = 'dead';

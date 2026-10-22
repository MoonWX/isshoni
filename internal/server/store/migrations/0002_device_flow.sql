-- 0002_device_flow.sql (M2): refresh-token rotation (docs/m2/02-device-auth.md §4, §6.2).

-- Hash of the refresh token whose exchange issued this token (access or refresh). NULL for the pair a device-code
-- grant issued. The grace paths of §6.2 delete the access token of one exchange by it, and check by it that a child
-- came from its parent's own exchange (a superseded child's successor has another issued_by).
ALTER TABLE device_tokens ADD COLUMN issued_by BLOB;

-- Refresh tokens, once spent: HMAC of the exchangeId the client sent with the request that spent it. NULL when the
-- client sent none. A replay with the same exchangeId is the client's own retry (§6.2).
ALTER TABLE device_tokens ADD COLUMN exchange_hash BLOB;

CREATE INDEX device_tokens_issued_by ON device_tokens(issued_by);

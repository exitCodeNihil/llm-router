-- allow any OIDC identity provider (Zitadel, Keycloak, Okta, ...) as a token
-- issuer, not just Azure Entra and GCP
ALTER TABLE token_issuers DROP CONSTRAINT token_issuers_type_check;
ALTER TABLE token_issuers ADD CONSTRAINT token_issuers_type_check CHECK (type IN ('entra','gcp','oidc'));

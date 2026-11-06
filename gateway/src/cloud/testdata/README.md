Throwaway RSA keys for the Google service-account tests (`gcp.rs`). They sign
nothing outside the tests and grant access to nothing. Secret scanners may flag
them: allowlist this folder.

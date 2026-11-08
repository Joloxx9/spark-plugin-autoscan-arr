# Security policy

Use GitHub's private vulnerability reporting on the repository Security tab to report vulnerabilities. Do not include API keys or private Silo configuration in public issues.

The latest release is supported. CI runs the pinned `govulncheck` tool before release. The plugin contacts only the Sonarr or Radarr server bound to the Autoscan source, with the API key Silo resolves for each poll. It stores no credentials and never puts the key or the full request URL in error messages.

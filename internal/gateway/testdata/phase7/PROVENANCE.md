# Phase 7 DTO Golden Provenance

These exact JSON fixtures pin gateway-owned output against the stable Emby SDK
4.9.5.0 OpenAPI contract at commit
`bdd0dd7c0801f6e069dff2795d80cddae6f91791` (2026-05-18).

Forward-compatibility was compared with Emby SDK 4.10.0.15-Beta at commit
`25947520d6ac5ad89fb8a2db34bf480ab589888e` (2026-06-18).

The fixtures intentionally retain the gateway's current exact field names,
omission rules, explicit false/zero values, and required non-null arrays. Large
or extensible BaseItem, DeviceProfile, and DisplayPreferences documents are not
closed goldens; their RawMessage preservation is asserted semantically in Go
tests.

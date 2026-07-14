# Emby Web 4.9.5.0 catalog provenance

Metadata-only provenance for the built-in production catalog
`emby-web-4.9.5.0`. This document records how the catalog JSON was reproduced.
It is **not** an Emby license grant and does **not** authorize redistribution of
official Emby Web dashboard asset bytes.

## Identity

| Field | Value |
| --- | --- |
| Catalog ID | `emby-web-4.9.5.0` |
| Version | `4.9.5.0` |
| Source image | `emby/embyserver` |
| Source image digest (linux/arm64 child) | `sha256:1e76e14a9c99507eb9f54361126f22c4658fc1588b2a710a99ba42f2335ff59a` |
| Parent multi-arch list digest (provenance only) | `sha256:734a6f03c7c783a9e566b08d09a2b6376f41229ff29f032a7e00302e0be98f8a` |
| Platform | `linux/arm64` |
| Catalog digest (SHA-256 of exact JSON bytes) | `e87b73c3dcfb138d9db1a9ccfb8ea9824514c9fd311615d1df9d25c690bdb87e` |
| Entry count | 868 (864 image dashboard-ui regular files + 4 runtime-only modules) |
| Aggregate prepared-tree size | `34213487` bytes |
| Reproduction date | `2026-07-14` |
| Host Go | `go1.26.4 darwin/arm64` |
| Host Docker | client/server `29.4.0` |

## Acquisition procedure

1. Pull the parent multi-arch list
   `emby/embyserver@sha256:734a6f03c7c783a9e566b08d09a2b6376f41229ff29f032a7e00302e0be98f8a`
   and select the **linux/arm64** child manifest
   `sha256:1e76e14a9c99507eb9f54361126f22c4658fc1588b2a710a99ba42f2335ff59a`.
2. Extract `/system/dashboard-ui` from that child image into an outside-repo work
   directory. Inventory regular files only; reject symlinks and special nodes.
3. Confirm disk cardinality **864**, zero symlinks/specials, and that the four
   runtime-only module paths are **absent** from the image tree.
4. Start **two** independently initialized containers from the same child
   manifest with empty `/config` volumes and no setup/wizard calls.
5. From each container, `GET` the four runtime modules with:
   - User-Agent: `Emby-Catalog-Repro/1.0`
   - `Accept-Encoding: identity`
   - redirects disabled
   - recorded method, path, status, size, and SHA-256
6. Require byte identity across both containers for every runtime module.
7. Merge the 864 image files and four runtime modules into a prepared 868-file
   tree outside the repository (aggregate size `34213487` bytes).
8. Generate canonical catalog JSON with the maintainer generator, then run the
   independent verifier with expected identity and digest pins.

Official asset bytes remain outside Git, CI artifacts, release archives, and
gateway image layers. Only path/size/hash/media/cache metadata is committed.

## Runtime acquisition records

All requests used method `GET`, User-Agent `Emby-Catalog-Repro/1.0`,
`Accept-Encoding: identity`, and no redirects. Both containers returned status
`200` with identical sizes and hashes.

| Path | Size | SHA-256 |
| --- | ---: | --- |
| `/web/modules/apphost.js` | 10540 | `11cb7865e7e09be7e4c89d963245dc7142a496f68c1b000b6d6edd61ca0fd9b8` |
| `/web/modules/input/keyboard.js` | 19867 | `01871146aea79bb7f7366bbf82881acef11bcc9a06a25b596493526f45dc90ce` |
| `/web/modules/virtual-scroller/virtual-scroller.js` | 33476 | `62b53c64e031db786a8b30017061f83bce8e46dc0a33804eaab4ca9dfb41f5ee` |
| `/web/modules/virtual-scroller/virtual-scroller.css` | 233 | `66d2f49974cec517543e106e6cdb89f2a280d654a2490a44b203b0559236fb9e` |

Catalog-relative paths omit the `/web/` prefix:

| Catalog path | SHA-256 |
| --- | --- |
| `modules/apphost.js` | `11cb7865e7e09be7e4c89d963245dc7142a496f68c1b000b6d6edd61ca0fd9b8` |
| `modules/input/keyboard.js` | `01871146aea79bb7f7366bbf82881acef11bcc9a06a25b596493526f45dc90ce` |
| `modules/virtual-scroller/virtual-scroller.js` | `62b53c64e031db786a8b30017061f83bce8e46dc0a33804eaab4ca9dfb41f5ee` |
| `modules/virtual-scroller/virtual-scroller.css` | `66d2f49974cec517543e106e6cdb89f2a280d654a2490a44b203b0559236fb9e` |

## Evidence digests (outside repository)

These SHA-256 digests fingerprint the temporary reproduction evidence files used
during publication. The evidence itself is not committed.

| Evidence file | SHA-256 |
| --- | --- |
| `image-counts.txt` | `75d78e2f19e6c1f0c1ee34876bdbb6d855aa80711c90de80cb2d7dea84e7e359` |
| `image-path-presence.txt` | `b18628aa86658d0fbb7be5a317886f8d8b46cdadde970e8c4170025e3e1d57a4` |
| `image-regular-file-digests.txt` | `2762284aaa7814d3956208d1e242bec67dec8ebd78e41ffeef5a5518dad0f3ec` |
| `prepared-counts.txt` | `f5ce581fa21ccdae1d1564e385dbd8e21baca98bd52cd372a9367489b079b6f2` |
| `prepared-regular-file-digests.txt` | `40c020482ba7c1ef399b8ecfd5cba081f05c5dad4eef72d1814422fa2744c5b0` |
| `runtime-runtime-records.json` | `9017c9aa962824c22986ea6c7980158a4f6ed56bffcc9dee273db4dc65f6431f` |
| `runtime-hash-comparison.txt` | `a4a133d65a3eac5fcf9b175bc04c2cff70e7b3b1f2f1f74c6251adba8561d73e` |
| `runtime-cross-container-equality.txt` | `2ad8ae2bfd92d1f0c4c85169a938873a4939a43a717ec8b38f3807a11d3005cd` |
| `platform-verification.txt` | `71f77e39b45ae365adecf045f90b9e205e81a1fdbc7aa2f9ca0bea54051f4599` |

## Maintainer tools

```sh
# Generate (digest printed on stderr)
mise exec -- go run ./tools/embywebcatalog/generate \
  --tree /path/to/prepared-868 \
  --id emby-web-4.9.5.0 \
  --version 4.9.5.0 \
  --source-image emby/embyserver \
  --source-image-digest sha256:1e76e14a9c99507eb9f54361126f22c4658fc1588b2a710a99ba42f2335ff59a \
  --out internal/embyweb/catalogs/emby-web-4.9.5.0.json

# Independent verify with expected identity + digest pins
mise exec -- go run ./tools/embywebcatalog/verify-independent \
  --tree /path/to/prepared-868 \
  --catalog internal/embyweb/catalogs/emby-web-4.9.5.0.json \
  --expect-id emby-web-4.9.5.0 \
  --expect-version 4.9.5.0 \
  --expect-source-image emby/embyserver \
  --expect-source-image-digest sha256:1e76e14a9c99507eb9f54361126f22c4658fc1588b2a710a99ba42f2335ff59a \
  --expect-digest e87b73c3dcfb138d9db1a9ccfb8ea9824514c9fd311615d1df9d25c690bdb87e
```

## Owner risk acceptance

The project owner authorized publication of this **catalog metadata** and the
technical reproduction procedure. That authorization is recorded as **owner risk
acceptance** for shipping path/hash metadata in this repository. It is **not** a
legal determination, license grant, or permission from Emby to redistribute
official dashboard asset bytes.

Operators who install must supply a **legally obtained** prepared source tree,
archive, or URL that matches this catalog. The gateway never downloads or embeds
official bytes automatically.

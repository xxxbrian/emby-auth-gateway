package embyweb

import _ "embed"

// Production catalog metadata is embedded as exact committed JSON bytes.
// Official Emby Web asset bytes are never embedded or redistributed.

//go:embed catalogs/emby-web-4.9.5.0.json
var catalogEmbyWeb4950JSON []byte

// Hard-coded pin of the exact embedded catalog bytes (lowercase 64-hex SHA-256).
// Construction fails closed if the embedded file is altered without updating this pin.
const catalogEmbyWeb4950Digest = "e87b73c3dcfb138d9db1a9ccfb8ea9824514c9fd311615d1df9d25c690bdb87e"

// productionCatalogDeclarations is the immutable built-in catalog set.
// Keep package-private: no CLI/catalog-file/digest override path exists.
func productionCatalogDeclarations() []catalogDeclaration {
	return []catalogDeclaration{{
		Bytes:          catalogEmbyWeb4950JSON,
		ExpectedDigest: catalogEmbyWeb4950Digest,
	}}
}

package embyweb

import (
	"fmt"
	"sync"
)

// catalogDeclaration pins exact committed catalog bytes and the expected digest
// of those bytes. Construction verifies the pin before accepting the catalog.
type catalogDeclaration struct {
	Bytes          []byte
	ExpectedDigest string // lowercase 64-hex SHA-256 of Bytes
}

// catalogRegistry is an immutable index of trusted catalogs by ID and full
// digest. Lookups never mutate the registry. Production uses an empty registry.
type catalogRegistry struct {
	byDigest  map[string]*trustedCatalog
	byID      map[string]*trustedCatalog
	byRelease map[string]*trustedCatalog
}

// productionRegistry is empty: no official catalog ships until legal/reproduction
// gates pass. Public New always uses this registry.
var (
	productionRegistry     *catalogRegistry
	productionRegistryOnce sync.Once
)

func getProductionRegistry() *catalogRegistry {
	productionRegistryOnce.Do(func() {
		reg, err := newCatalogRegistry(nil)
		if err != nil {
			panic("embyweb: empty production registry construction failed: " + err.Error())
		}
		productionRegistry = reg
	})
	return productionRegistry
}

// newCatalogRegistry builds an immutable registry from declarations. Each
// declaration's bytes are hashed and must equal ExpectedDigest; catalogs are
// parsed and validated. Duplicate IDs, digests, or derived release names fail.
// There is no production path to insert arbitrary catalogs.
func newCatalogRegistry(decls []catalogDeclaration) (*catalogRegistry, error) {
	reg := &catalogRegistry{
		byDigest:  make(map[string]*trustedCatalog, len(decls)),
		byID:      make(map[string]*trustedCatalog, len(decls)),
		byRelease: make(map[string]*trustedCatalog, len(decls)),
	}
	for i, d := range decls {
		if len(d.Bytes) == 0 {
			return nil, fmt.Errorf("registry declaration[%d]: empty bytes", i)
		}
		if !validSHA256Hex(d.ExpectedDigest) {
			return nil, fmt.Errorf("registry declaration[%d]: expected digest is not lowercase 64-hex", i)
		}
		got := catalogDigest(d.Bytes)
		if got != d.ExpectedDigest {
			return nil, fmt.Errorf("registry declaration[%d]: pin mismatch (got %s want %s)", i, got, d.ExpectedDigest)
		}
		tc, err := parseCatalog(d.Bytes)
		if err != nil {
			return nil, fmt.Errorf("registry declaration[%d]: %w", i, err)
		}
		if tc.Digest != d.ExpectedDigest {
			return nil, fmt.Errorf("registry declaration[%d]: parsed digest mismatch", i)
		}
		if _, dup := reg.byDigest[tc.Digest]; dup {
			return nil, fmt.Errorf("registry declaration[%d]: duplicate digest %s", i, tc.Digest)
		}
		if _, dup := reg.byID[tc.Catalog.ID]; dup {
			return nil, fmt.Errorf("registry declaration[%d]: duplicate catalog id %q", i, tc.Catalog.ID)
		}
		if _, dup := reg.byRelease[tc.Release]; dup {
			return nil, fmt.Errorf("registry declaration[%d]: duplicate release %q", i, tc.Release)
		}
		reg.byDigest[tc.Digest] = tc
		reg.byID[tc.Catalog.ID] = tc
		reg.byRelease[tc.Release] = tc
	}
	return reg, nil
}

// lookupByDigest returns the trusted catalog for a full 64-hex digest, or
// ErrUntrustedCatalog when unknown (including empty production registry).
func (r *catalogRegistry) lookupByDigest(digest string) (*trustedCatalog, error) {
	if r == nil {
		return nil, ErrUntrustedCatalog
	}
	tc, ok := r.byDigest[digest]
	if !ok {
		return nil, ErrUntrustedCatalog
	}
	return tc, nil
}

// lookupByID returns the trusted catalog for a catalog ID, or ErrCatalogLegalGate
// when the production/empty registry has no such ID.
func (r *catalogRegistry) lookupByID(id string) (*trustedCatalog, error) {
	if r == nil {
		return nil, ErrCatalogLegalGate
	}
	tc, ok := r.byID[id]
	if !ok {
		return nil, ErrCatalogLegalGate
	}
	return tc, nil
}

// len returns the number of trusted catalogs (for tests).
func (r *catalogRegistry) len() int {
	if r == nil {
		return 0
	}
	return len(r.byDigest)
}

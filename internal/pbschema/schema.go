// Package pbschema owns the gateway's canonical PocketBase schema.
package pbschema

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
)

// ErrUnsupportedSchema identifies a database that is neither pristine nor the
// exact schema required by this gateway version.
var ErrUnsupportedSchema = errors.New("unsupported PocketBase schema")

var errPhysicalSchemaMismatch = errors.New("physical schema mismatch")

var requiredNames = []string{
	"gateway_sessions", "audit_logs", "playback_events", "user_item_data",
	"item_child_counts", "display_preferences", "path_policies", "upstream_sources", "upstream_endpoints",
}

// Ensure initializes a pristine PocketBase database or validates an existing
// gateway schema. Existing databases are never modified.
func Ensure(app core.App) error {
	snapshot, err := loadCollections(app)
	if err != nil {
		return err
	}
	state, err := classify(app, snapshot)
	if err != nil {
		return err
	}
	if state == existingSchema {
		return validate(app)
	}
	if state != freshSchema {
		return fmt.Errorf("%w: required collections are partial or users is not pristine", ErrUnsupportedSchema)
	}
	return app.RunInTransaction(func(tx core.App) error {
		snapshot, err := loadCollections(tx)
		if err != nil {
			return err
		}
		state, err := classify(tx, snapshot)
		if err != nil {
			return err
		}
		switch state {
		case existingSchema:
			return validateSnapshot(tx, snapshot)
		case freshSchema:
			users := snapshot.byName["users"]
			configureUsers(users)
			if err := tx.Save(users); err != nil {
				return err
			}
			users, err = collectionByExactName(tx, "users")
			if err != nil {
				return err
			}
			ids := map[string]string{"users": users.Id}
			// Save and refetch each collection before using its generated ID in a relation.
			reserved := freshReservedNames(snapshot)
			for _, collection := range []*core.Collection{childCounts(), policies(), sources()} {
				assignFreshID(collection, reserved)
				if err := tx.Save(collection); err != nil {
					return err
				}
				persisted, err := collectionByExactName(tx, collection.Name)
				if err != nil {
					return fmt.Errorf("persist collection %q identity: %w", collection.Name, err)
				}
				ids[persisted.Name] = persisted.Id
			}
			for _, collection := range []*core.Collection{sessions(ids["users"]), audit(ids["users"]), playback(ids["users"]), userData(ids["users"]), preferences(ids["users"]), endpoints(ids["upstream_sources"])} {
				assignFreshID(collection, reserved)
				if err := tx.Save(collection); err != nil {
					return err
				}
			}
			return validate(tx)
		default:
			return errors.New("concurrent schema initialization left a partial schema")
		}
	})
}

type schemaState uint8

const (
	unsupportedSchema schemaState = iota
	freshSchema
	existingSchema
)

type collectionSnapshot struct {
	byName map[string]*core.Collection
	byID   map[string]*core.Collection
}

func loadCollections(app core.App) (*collectionSnapshot, error) {
	all, err := app.FindAllCollections()
	if err != nil {
		return nil, fmt.Errorf("list collections: %w", err)
	}
	snapshot := &collectionSnapshot{byName: make(map[string]*core.Collection, len(all)), byID: make(map[string]*core.Collection, len(all))}
	for _, collection := range all {
		if collection == nil || collection.Name == "" || collection.Id == "" {
			return nil, fmt.Errorf("%w: invalid collection identity", ErrUnsupportedSchema)
		}
		if _, exists := snapshot.byName[collection.Name]; exists {
			return nil, fmt.Errorf("%w: duplicate collection name %q", ErrUnsupportedSchema, collection.Name)
		}
		if _, exists := snapshot.byID[collection.Id]; exists {
			return nil, fmt.Errorf("%w: duplicate collection id %q", ErrUnsupportedSchema, collection.Id)
		}
		snapshot.byName[collection.Name], snapshot.byID[collection.Id] = collection, collection
	}
	for name, collection := range snapshot.byName {
		if !requiredCollectionName(name) {
			continue
		}
		if byID := snapshot.byID[name]; byID != nil && byID != collection {
			return nil, fmt.Errorf("%w: collection name %q collides with collection id", ErrUnsupportedSchema, name)
		}
	}
	if err := rejectRequiredIDNameCollisions(snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func rejectRequiredIDNameCollisions(snapshot *collectionSnapshot) error {
	for _, name := range append([]string{"users"}, requiredNames...) {
		if byID := snapshot.byID[name]; byID != nil && snapshot.byName[name] != byID {
			return fmt.Errorf("%w: required collection name %q collides with collection id", ErrUnsupportedSchema, name)
		}
	}
	for _, name := range append([]string{"users"}, requiredNames...) {
		collection := snapshot.byName[name]
		if collection != nil && snapshot.byName[collection.Id] != nil && snapshot.byName[collection.Id] != collection {
			return fmt.Errorf("%w: required collection id %q collides with collection name", ErrUnsupportedSchema, collection.Id)
		}
	}
	return nil
}

func collectionByExactName(app core.App, name string) (*core.Collection, error) {
	snapshot, err := loadCollections(app)
	if err != nil {
		return nil, err
	}
	collection := snapshot.byName[name]
	if collection == nil {
		return nil, fmt.Errorf("%w: collection %q missing", ErrUnsupportedSchema, name)
	}
	return collection, nil
}

func classify(app core.App, snapshot *collectionSnapshot) (schemaState, error) {
	users := snapshot.byName["users"]
	if users == nil {
		return unsupportedSchema, fmt.Errorf("%w: users missing", ErrUnsupportedSchema)
	}
	found := 0
	for _, name := range requiredNames {
		if snapshot.byName[name] != nil {
			found++
		}
	}
	if found == len(requiredNames) {
		return existingSchema, nil
	}
	if found != 0 {
		return unsupportedSchema, nil
	}
	count, err := app.CountRecords(users)
	if err != nil {
		return unsupportedSchema, fmt.Errorf("count users: %w", err)
	}
	if users.Id != "_pb_users_auth_" || count != 0 || !collectionEqual(users, pristineUsers()) || !validAuthSecrets(users) {
		return unsupportedSchema, nil
	}
	if err := validateCollectionRaw(app, users, pristineUsers()); err != nil {
		if errors.Is(err, errPhysicalSchemaMismatch) {
			return unsupportedSchema, nil
		}
		return unsupportedSchema, err
	}
	if err := validateTable(app, users); err != nil {
		if errors.Is(err, errPhysicalSchemaMismatch) {
			return unsupportedSchema, nil
		}
		return unsupportedSchema, err
	}
	return freshSchema, nil
}

func validate(app core.App) error {
	snapshot, err := loadCollections(app)
	if err != nil {
		return err
	}
	return validateSnapshot(app, snapshot)
}

func validateSnapshot(app core.App, snapshot *collectionSnapshot) error {
	users := snapshot.byName["users"]
	if users == nil {
		return fmt.Errorf("%w: users missing", ErrUnsupportedSchema)
	}
	if users.Id != "_pb_users_auth_" || !collectionEqual(users, configuredUsers()) || !validAuthSecrets(users) {
		return fmt.Errorf("%w: users differs from the canonical schema", ErrUnsupportedSchema)
	}
	ids, err := canonicalIDs(snapshot, users)
	if err != nil {
		return err
	}
	expected := collections(ids)
	for _, want := range expected {
		got := snapshot.byName[want.Name]
		if got == nil || !collectionEqual(got, want) {
			if got != nil {
				return fmt.Errorf("%w: collection %q differs: %s", ErrUnsupportedSchema, want.Name, collectionDifference(got, want))
			}
			return fmt.Errorf("%w: collection %q differs from the canonical schema", ErrUnsupportedSchema, want.Name)
		}
		if err := validateCollectionRaw(app, got, want); err != nil {
			if errors.Is(err, errPhysicalSchemaMismatch) {
				return fmt.Errorf("%w: %v", ErrUnsupportedSchema, err)
			}
			return fmt.Errorf("validate raw collection %q: %w", want.Name, err)
		}
		if err := validateTable(app, got); err != nil {
			if errors.Is(err, errPhysicalSchemaMismatch) {
				return fmt.Errorf("%w: %v", ErrUnsupportedSchema, err)
			}
			return fmt.Errorf("validate table %q: %w", want.Name, err)
		}
	}
	if err := validateCollectionRaw(app, users, configuredUsers()); err != nil {
		if errors.Is(err, errPhysicalSchemaMismatch) {
			return fmt.Errorf("%w: %v", ErrUnsupportedSchema, err)
		}
		return fmt.Errorf("validate raw users: %w", err)
	}
	if err := validateTable(app, users); err != nil {
		if errors.Is(err, errPhysicalSchemaMismatch) {
			return fmt.Errorf("%w: %v", ErrUnsupportedSchema, err)
		}
		return fmt.Errorf("validate users table: %w", err)
	}
	return nil
}

func collectionDifference(got, want *core.Collection) string {
	if len(got.Fields) != len(want.Fields) {
		return fmt.Sprintf("field count %d != %d", len(got.Fields), len(want.Fields))
	}
	for i := range want.Fields {
		if !fieldEqual(got.Fields[i], want.Fields[i]) {
			return fmt.Sprintf("field %d %q != %q", i, got.Fields[i].GetName(), want.Fields[i].GetName())
		}
	}
	return "metadata or indexes"
}

func canonicalIDs(snapshot *collectionSnapshot, users *core.Collection) (map[string]string, error) {
	ids := map[string]string{"users": users.Id}
	seen := map[string]string{users.Id: "users"}
	for _, name := range requiredNames {
		collection := snapshot.byName[name]
		if collection == nil || !validApplicationID(collection.Id) {
			return nil, fmt.Errorf("%w: invalid collection identity for %q", ErrUnsupportedSchema, name)
		}
		if previous, ok := seen[collection.Id]; ok {
			return nil, fmt.Errorf("%w: collections %q and %q share an id", ErrUnsupportedSchema, previous, name)
		}
		if snapshot.byID[collection.Id] != collection {
			return nil, fmt.Errorf("%w: ambiguous collection identity for %q", ErrUnsupportedSchema, name)
		}
		seen[collection.Id], ids[name] = name, collection.Id
	}
	return ids, nil
}

func validApplicationID(id string) bool {
	return len(id) > 0 && len(id) <= 100 && core.DefaultIdRegex.MatchString(id)
}

var generateApplicationID = core.GenerateDefaultRandomId

func assignFreshID(collection *core.Collection, reserved map[string]struct{}) {
	for {
		id := generateApplicationID()
		if validApplicationID(id) {
			if _, exists := reserved[id]; !exists {
				collection.Id, reserved[id] = id, struct{}{}
				return
			}
		}
	}
}

func freshReservedNames(snapshot *collectionSnapshot) map[string]struct{} {
	result := make(map[string]struct{}, len(snapshot.byID)+len(snapshot.byName)+len(requiredNames)+1)
	for id := range snapshot.byID {
		result[id] = struct{}{}
	}
	for name := range snapshot.byName {
		result[name] = struct{}{}
	}
	for _, name := range append([]string{"users"}, requiredNames...) {
		result[name] = struct{}{}
	}
	return result
}

func requiredCollectionName(name string) bool {
	if name == "users" {
		return true
	}
	for _, required := range requiredNames {
		if name == required {
			return true
		}
	}
	return false
}

func pristineUsers() *core.Collection {
	u := core.NewAuthCollection("users", "_pb_users_auth_")
	owner := "id = @request.auth.id"
	u.ListRule, u.ViewRule = types.Pointer(owner), types.Pointer(owner)
	u.CreateRule, u.UpdateRule, u.DeleteRule = types.Pointer(""), types.Pointer(owner), types.Pointer(owner)
	u.Fields.Add(&core.TextField{Name: "name", Max: 255})
	u.Fields.Add(&core.FileField{Name: "avatar", MaxSelect: 1, MimeTypes: []string{"image/jpeg", "image/png", "image/svg+xml", "image/gif", "image/webp"}})
	u.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	u.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
	u.OAuth2.MappedFields.Name, u.OAuth2.MappedFields.AvatarURL = "name", "avatar"
	return u
}

func configuredUsers() *core.Collection {
	u := pristineUsers()
	configureUsers(u)
	return u
}

func configureUsers(users *core.Collection) {
	lock(users)
	users.PasswordAuth.Enabled = false
	users.PasswordAuth.IdentityFields = []string{"username"}
	users.Fields.Add(&core.TextField{Name: "username", Required: true, Max: 255})
	users.Fields.Add(&core.TextField{Name: "synthetic_user_id", Required: true, Max: 80})
	users.Fields.Add(&core.BoolField{Name: "enabled"})
	users.AddIndex("idx_users_username", true, "username", "")
	users.AddIndex("idx_users_synthetic", true, "synthetic_user_id", "")
}

func lock(c *core.Collection) {
	c.ListRule, c.ViewRule, c.CreateRule, c.UpdateRule, c.DeleteRule = nil, nil, nil, nil, nil
}

func collectionEqual(got, want *core.Collection) bool {
	if got == nil || got.Name != want.Name || got.Type != want.Type || got.System != want.System || !rulesEqual(got, want) || len(got.Fields) != len(want.Fields) {
		return false
	}
	for i := range want.Fields {
		if !fieldEqual(got.Fields[i], want.Fields[i]) {
			return false
		}
	}
	return indexesEqual(got.Indexes, want.Indexes) && authEqual(got, want)
}

func rulesEqual(a, b *core.Collection) bool {
	return pointerEqual(a.ListRule, b.ListRule) && pointerEqual(a.ViewRule, b.ViewRule) && pointerEqual(a.CreateRule, b.CreateRule) && pointerEqual(a.UpdateRule, b.UpdateRule) && pointerEqual(a.DeleteRule, b.DeleteRule)
}
func pointerEqual(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func fieldEqual(a, b core.Field) bool {
	if a.Type() != b.Type() || a.GetName() != b.GetName() || a.GetSystem() != b.GetSystem() || a.GetHidden() != b.GetHidden() {
		return false
	}
	return reflect.DeepEqual(normalField(a), normalField(b))
}

func normalField(field core.Field) map[string]any {
	b, _ := json.Marshal(field)
	var result map[string]any
	_ = json.Unmarshal(b, &result)
	delete(result, "id")
	return result
}

func authEqual(a, b *core.Collection) bool {
	if !a.IsAuth() && !b.IsAuth() {
		return true
	}
	return reflect.DeepEqual(normalCollectionOptions(a), normalCollectionOptions(b))
}

func validAuthSecrets(c *core.Collection) bool {
	for _, secret := range []string{c.AuthToken.Secret, c.PasswordResetToken.Secret, c.EmailChangeToken.Secret, c.VerificationToken.Secret, c.FileToken.Secret} {
		if len(secret) < 30 || len(secret) > 255 {
			return false
		}
	}
	return true
}

func normalCollectionOptions(c *core.Collection) map[string]any {
	b, _ := json.Marshal(c)
	var result map[string]any
	_ = json.Unmarshal(b, &result)
	delete(result, "id")
	delete(result, "created")
	delete(result, "updated")
	delete(result, "fields")
	delete(result, "indexes")
	return result
}

func indexesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aa, bb := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(aa)
	sort.Strings(bb)
	return reflect.DeepEqual(aa, bb)
}

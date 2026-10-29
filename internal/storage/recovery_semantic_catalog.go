// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
)

var ErrRecoverySemanticCatalog = errors.New("unsupported recovery semantic catalogue")

const recoverySemanticCatalogSHA256 = "UNSEALED"
const recoverySemanticNamespace = "__runverge_recovery_target__"
const recoverySemanticMaxRecord = 256 << 10
const recoverySemanticMaxBytes = 8 << 20

type RecoverySemanticCatalog struct {
	SchemaSHA256           string                       `json:"schema_sha256"`
	DatabaseCollation      string                       `json:"database_collation"`
	DatabaseCType          string                       `json:"database_ctype"`
	DatabaseLocaleProvider string                       `json:"database_locale_provider"`
	DatabaseLocale         string                       `json:"database_locale"`
	Relations              []RecoverySemanticRelation   `json:"relations"`
	Constraints            []RecoverySemanticConstraint `json:"constraints"`
	Indexes                []RecoverySemanticIndex      `json:"indexes"`
	Columns                []RecoverySemanticColumn     `json:"columns"`
	Triggers               []RecoverySemanticTrigger    `json:"triggers"`
	Functions              []RecoverySemanticFunction   `json:"functions"`
}
type RecoverySemanticRelation struct {
	Table            string   `json:"table"`
	Kind             string   `json:"kind"`
	Persistence      string   `json:"persistence"`
	RowSecurity      bool     `json:"row_security"`
	ForceRowSecurity bool     `json:"force_row_security"`
	ReplicaIdentity  string   `json:"replica_identity"`
	Options          []string `json:"options"`
}
type RecoverySemanticRITrigger struct {
	Table          string `json:"table"`
	FunctionSchema string `json:"function_schema"`
	FunctionName   string `json:"function_name"`
	Type           int    `json:"type"`
	Enabled        string `json:"enabled"`
	Deferrable     bool   `json:"deferrable"`
	Deferred       bool   `json:"deferred"`
}
type RecoverySemanticConstraint struct {
	Table        string                      `json:"table"`
	Name         string                      `json:"name"`
	Type         string                      `json:"type"`
	Keys         []string                    `json:"keys"`
	RefSchema    string                      `json:"ref_schema"`
	RefTable     string                      `json:"ref_table"`
	RefKeys      []string                    `json:"ref_keys"`
	MatchType    string                      `json:"match_type"`
	UpdateAction string                      `json:"update_action"`
	DeleteAction string                      `json:"delete_action"`
	Deferrable   bool                        `json:"deferrable"`
	Deferred     bool                        `json:"deferred"`
	Validated    bool                        `json:"validated"`
	Enforced     bool                        `json:"enforced"`
	NoInherit    bool                        `json:"no_inherit"`
	IsLocal      bool                        `json:"is_local"`
	Definition   string                      `json:"definition"`
	RITriggers   []RecoverySemanticRITrigger `json:"ri_triggers"`
}
type RecoverySemanticIndex struct {
	Table            string   `json:"table"`
	Name             string   `json:"name"`
	Method           string   `json:"method"`
	Keys             []string `json:"keys"`
	Includes         []string `json:"includes"`
	OpClasses        []string `json:"opclasses"`
	Collations       []string `json:"collations"`
	Unique           bool     `json:"unique"`
	Primary          bool     `json:"primary"`
	Exclusion        bool     `json:"exclusion"`
	Valid            bool     `json:"valid"`
	Ready            bool     `json:"ready"`
	Live             bool     `json:"live"`
	Immediate        bool     `json:"immediate"`
	NullsNotDistinct bool     `json:"nulls_not_distinct"`
	Clustered        bool     `json:"clustered"`
	ReplicaIdentity  bool     `json:"replica_identity"`
	Definition       string   `json:"definition"`
	Expressions      string   `json:"expressions"`
	Predicate        string   `json:"predicate"`
	Options          []string `json:"options"`
}
type RecoverySemanticColumn struct {
	Table           string `json:"table"`
	Name            string `json:"name"`
	Ordinal         int    `json:"ordinal"`
	TypeSchema      string `json:"type_schema"`
	TypeName        string `json:"type_name"`
	TypeModifier    int    `json:"type_modifier"`
	Dimensions      int    `json:"dimensions"`
	NotNull         bool   `json:"not_null"`
	Generated       string `json:"generated"`
	Identity        string `json:"identity"`
	CollationSchema string `json:"collation_schema"`
	CollationName   string `json:"collation_name"`
	Expression      string `json:"expression"`
	// Captured from the same trusted target query. Only use after catalogue validation.
	// Expression is its schema-normalized equivalent used in the compiled pin.
	RawExpression string `json:"-"`
}
type RecoverySemanticTrigger struct {
	Table             string `json:"table"`
	Name              string `json:"name"`
	Type              int    `json:"type"`
	Enabled           string `json:"enabled"`
	FunctionSchema    string `json:"function_schema"`
	FunctionName      string `json:"function_name"`
	FunctionArguments string `json:"function_arguments"`
	ArgumentsHex      string `json:"arguments_hex"`
	When              string `json:"when"`
	Definition        string `json:"definition"`
}
type RecoverySemanticFunction struct {
	Name            string   `json:"name"`
	Arguments       string   `json:"arguments"`
	Result          string   `json:"result"`
	Language        string   `json:"language"`
	Kind            string   `json:"kind"`
	SecurityDefiner bool     `json:"security_definer"`
	Strict          bool     `json:"strict"`
	Leakproof       bool     `json:"leakproof"`
	ReturnsSet      bool     `json:"returns_set"`
	Volatility      string   `json:"volatility"`
	Parallel        string   `json:"parallel"`
	Cost            float64  `json:"cost"`
	Rows            float64  `json:"rows"`
	Configuration   []string `json:"configuration"`
	Source          string   `json:"source"`
	Definition      string   `json:"definition"`
}

// Both functions inspect only a caller-owned transaction and fixed target schema.
// Callers must rollback on any error; neither function runs migration/archive SQL.
func validateRecoverySemanticCatalog(ctx context.Context, tx pgx.Tx, schema string) (RecoverySemanticCatalog, error) {
	catalog, err := readRecoverySemanticCatalog(ctx, tx, schema)
	if err != nil {
		return RecoverySemanticCatalog{}, err
	}
	raw, err := json.Marshal(catalog)
	if err != nil || len(raw) > recoverySemanticMaxBytes || Hash(raw) != recoverySemanticCatalogSHA256 {
		return RecoverySemanticCatalog{}, ErrRecoverySemanticCatalog
	}
	// Recovery receipts/fences are operator metadata. A relation-shape pin alone
	// does not detect ACL grants; reject table or column write grants to any
	// non-owner role before reading or trusting these records. Database owners
	// and superusers remain an external operational trust boundary.
	var privateMetadata bool
	err = tx.QueryRow(ctx, `SELECT pg_catalog.count(*)=2 AND NOT pg_catalog.bool_or(
 EXISTS(SELECT 1 FROM pg_catalog.aclexplode(COALESCE(c.relacl,pg_catalog.acldefault('r',c.relowner))) permission
 WHERE permission.grantee<>c.relowner AND permission.privilege_type IN('INSERT','UPDATE','DELETE','TRUNCATE'))
 OR EXISTS(SELECT 1 FROM pg_catalog.pg_attribute a CROSS JOIN LATERAL pg_catalog.aclexplode(a.attacl) permission
 WHERE a.attrelid=c.oid AND permission.grantee<>c.relowner AND permission.privilege_type IN('INSERT','UPDATE')))
 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname=$1 AND c.relname IN('recovery_import_receipts','recovery_historical_rows')`, schema).Scan(&privateMetadata)
	if err != nil || !privateMetadata {
		return RecoverySemanticCatalog{}, recoverySnapshotError(ctx, ErrRecoverySemanticCatalog)
	}
	if err = verifyRecoverySchemaTx(ctx, tx, schema); err != nil {
		return RecoverySemanticCatalog{}, recoverySnapshotError(ctx, ErrRecoverySemanticCatalog)
	}
	return catalog, nil
}
func readRecoverySemanticCatalog(ctx context.Context, tx pgx.Tx, schema string) (catalog RecoverySemanticCatalog, err error) {
	if schema == "" || len(schema) > 63 || strings.IndexByte(schema, 0) >= 0 {
		return catalog, ErrRecoverySemanticCatalog
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	fail := func() (RecoverySemanticCatalog, error) {
		return RecoverySemanticCatalog{}, recoverySnapshotError(ctx, ErrRecoverySemanticCatalog)
	}
	var previous string
	if err = tx.QueryRow(ctx, "SELECT pg_catalog.current_setting('search_path')").Scan(&previous); err != nil {
		return fail()
	}
	path := "pg_catalog," + pgx.Identifier{schema}.Sanitize() + ",pg_temp"
	if _, err = tx.Exec(ctx, "SELECT pg_catalog.set_config('search_path',$1,true)", path); err != nil {
		return fail()
	}
	defer func() {
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), time.Second)
		defer restoreCancel()
		if _, restoreErr := tx.Exec(restoreCtx, "SELECT pg_catalog.set_config('search_path',$1,true)", previous); restoreErr != nil && err == nil {
			catalog = RecoverySemanticCatalog{}
			err = ErrRecoverySemanticCatalog
		}
	}()
	if _, err = tx.Exec(ctx, "SELECT pg_catalog.pg_advisory_xact_lock_shared(841932079)"); err != nil {
		return fail()
	}
	inventory, e := recoverySnapshotInventory(ctx, tx, schema, true)
	if e != nil {
		return fail()
	}
	inventoryRaw, _ := json.Marshal(inventory)
	if Hash(inventoryRaw) != recoverySnapshotInventorySHA256 {
		return fail()
	}
	// Current trusted migrations create no operators, custom scalar types, rules,
	// collations or conversions. Reject these resolution/rewrite dependencies
	// even when their implementation refers only to existing pg_catalog code.
	// The first fresh-DB profile also refuses non-engine custom types anywhere:
	// pg_get_* deparsers can invoke a Const type's output function. Ordinary
	// table rowtypes and their exact arrays have builtin record/array I/O; every
	// non-engine scalar/domain/range/enum/manual-composite type is refused, so
	// nested record fields cannot introduce an unreviewed output implementation.
	var unsupported bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM pg_catalog.pg_operator x JOIN pg_catalog.pg_namespace n ON n.oid=x.oprnamespace WHERE n.nspname=$1
 UNION ALL SELECT 1 FROM pg_catalog.pg_opclass x JOIN pg_catalog.pg_namespace n ON n.oid=x.opcnamespace WHERE n.nspname=$1
 UNION ALL SELECT 1 FROM pg_catalog.pg_opfamily x JOIN pg_catalog.pg_namespace n ON n.oid=x.opfnamespace WHERE n.nspname=$1
 UNION ALL SELECT 1 FROM pg_catalog.pg_collation x JOIN pg_catalog.pg_namespace n ON n.oid=x.collnamespace WHERE n.nspname=$1
 UNION ALL SELECT 1 FROM pg_catalog.pg_conversion x JOIN pg_catalog.pg_namespace n ON n.oid=x.connamespace WHERE n.nspname=$1
 UNION ALL SELECT 1 FROM pg_catalog.pg_rewrite x JOIN pg_catalog.pg_class r ON r.oid=x.ev_class JOIN pg_catalog.pg_namespace n ON n.oid=r.relnamespace WHERE n.nspname=$1
 UNION ALL SELECT 1 FROM pg_catalog.pg_constraint co JOIN pg_catalog.pg_class target ON target.oid=co.confrelid JOIN pg_catalog.pg_namespace n ON n.oid=target.relnamespace JOIN pg_catalog.pg_class origin ON origin.oid=co.conrelid WHERE co.contype='f' AND n.nspname=$1 AND origin.relnamespace<>target.relnamespace
 UNION ALL SELECT 1 FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname NOT IN('pg_catalog','information_schema')
 AND NOT (t.typtype='c' AND t.typinput='pg_catalog.record_in'::pg_catalog.regproc AND t.typoutput='pg_catalog.record_out'::pg_catalog.regproc
 AND EXISTS(SELECT 1 FROM pg_catalog.pg_class r WHERE r.oid=t.typrelid AND r.reltype=t.oid AND r.relnamespace=n.oid AND r.relkind='r'))
 AND NOT (t.typtype='b' AND t.typinput='pg_catalog.array_in'::pg_catalog.regproc AND t.typoutput='pg_catalog.array_out'::pg_catalog.regproc
 AND EXISTS(SELECT 1 FROM pg_catalog.pg_type e JOIN pg_catalog.pg_class r ON r.oid=e.typrelid WHERE t.typelem=e.oid AND e.typtype='c' AND r.reltype=e.oid AND r.relnamespace=n.oid AND r.relkind='r' AND e.typarray=t.oid))
 )`, schema).Scan(&unsupported); err != nil || unsupported {
		return fail()
	}
	var serverVersion int
	var encoding string
	if err = tx.QueryRow(ctx, `SELECT pg_catalog.current_setting('server_version_num')::integer,pg_catalog.current_setting('server_encoding'),d.datcollate,d.datctype,d.datlocprovider::text,COALESCE(d.datlocale,'') FROM pg_catalog.pg_database d WHERE d.datname=pg_catalog.current_database()`).Scan(&serverVersion, &encoding, &catalog.DatabaseCollation, &catalog.DatabaseCType, &catalog.DatabaseLocaleProvider, &catalog.DatabaseLocale); err != nil || serverVersion < 180000 || serverVersion >= 190000 || encoding != "UTF8" {
		return fail()
	}
	catalog.SchemaSHA256 = EmbeddedSchemaSHA256()
	total := 0
	catalog.Relations, err = recoverySemanticRows[RecoverySemanticRelation](ctx, tx, `SELECT pg_catalog.jsonb_build_object('table',c.relname,'kind',c.relkind::text,'persistence',c.relpersistence::text,'row_security',c.relrowsecurity,'force_row_security',c.relforcerowsecurity,'replica_identity',c.relreplident::text,'options',COALESCE(c.reloptions,ARRAY[]::text[])) AS value FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relkind IN('r','p','f','v','m')`, schema, 256, &total)
	if err != nil {
		return fail()
	}
	catalog.Constraints, err = recoverySemanticRows[RecoverySemanticConstraint](ctx, tx, `SELECT pg_catalog.jsonb_build_object(
 'table',c.relname,'name',co.conname,'type',co.contype::text,
 'keys',ARRAY(SELECT a.attname::text FROM pg_catalog.unnest(co.conkey) WITH ORDINALITY k(attnum,ord) JOIN pg_catalog.pg_attribute a ON a.attrelid=co.conrelid AND a.attnum=k.attnum ORDER BY k.ord),
 'ref_schema',COALESCE(rn.nspname,''),'ref_table',COALESCE(r.relname,''),
 'ref_keys',ARRAY(SELECT a.attname::text FROM pg_catalog.unnest(co.confkey) WITH ORDINALITY k(attnum,ord) JOIN pg_catalog.pg_attribute a ON a.attrelid=co.confrelid AND a.attnum=k.attnum ORDER BY k.ord),
 'match_type',co.confmatchtype::text,'update_action',co.confupdtype::text,'delete_action',co.confdeltype::text,
 'deferrable',co.condeferrable,'deferred',co.condeferred,'validated',co.convalidated,'enforced',co.conenforced,'no_inherit',co.connoinherit,'is_local',co.conislocal,
 'definition',pg_catalog.pg_get_constraintdef(co.oid,false),
 'ri_triggers',COALESCE((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('table',tc.relname,'function_schema',pn.nspname,'function_name',p.proname,'type',tr.tgtype,'enabled',tr.tgenabled::text,'deferrable',tr.tgdeferrable,'deferred',tr.tginitdeferred) ORDER BY tc.relname COLLATE "C",p.proname COLLATE "C",tr.tgtype)
 FROM pg_catalog.pg_trigger tr JOIN pg_catalog.pg_class tc ON tc.oid=tr.tgrelid JOIN pg_catalog.pg_proc p ON p.oid=tr.tgfoid JOIN pg_catalog.pg_namespace pn ON pn.oid=p.pronamespace WHERE tr.tgconstraint=co.oid AND tr.tgisinternal),'[]'::jsonb)) AS value
 FROM pg_catalog.pg_constraint co JOIN pg_catalog.pg_class c ON c.oid=co.conrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 LEFT JOIN pg_catalog.pg_class r ON r.oid=co.confrelid LEFT JOIN pg_catalog.pg_namespace rn ON rn.oid=r.relnamespace WHERE n.nspname=$1`, schema, 4096, &total)
	if err != nil {
		return fail()
	}
	catalog.Indexes, err = recoverySemanticRows[RecoverySemanticIndex](ctx, tx, `SELECT pg_catalog.jsonb_build_object(
 'table',t.relname,'name',c.relname,'method',am.amname,
 'keys',ARRAY(SELECT COALESCE(a.attname::text,'') FROM pg_catalog.unnest(i.indkey) WITH ORDINALITY k(attnum,ord) LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid=t.oid AND a.attnum=k.attnum WHERE k.ord<=i.indnkeyatts ORDER BY k.ord),
 'includes',ARRAY(SELECT a.attname::text FROM pg_catalog.unnest(i.indkey) WITH ORDINALITY k(attnum,ord) JOIN pg_catalog.pg_attribute a ON a.attrelid=t.oid AND a.attnum=k.attnum WHERE k.ord>i.indnkeyatts ORDER BY k.ord),
 'opclasses',ARRAY(SELECT pn.nspname||'.'||op.opcname FROM pg_catalog.unnest(i.indclass) WITH ORDINALITY k(oid,ord) JOIN pg_catalog.pg_opclass op ON op.oid=k.oid JOIN pg_catalog.pg_namespace pn ON pn.oid=op.opcnamespace ORDER BY k.ord),
 'collations',ARRAY(SELECT COALESCE(cn.nspname||'.'||cl.collname,'') FROM pg_catalog.unnest(i.indcollation) WITH ORDINALITY k(oid,ord) LEFT JOIN pg_catalog.pg_collation cl ON cl.oid=k.oid LEFT JOIN pg_catalog.pg_namespace cn ON cn.oid=cl.collnamespace ORDER BY k.ord),
 'unique',i.indisunique,'primary',i.indisprimary,'exclusion',i.indisexclusion,'valid',i.indisvalid,'ready',i.indisready,'live',i.indislive,'immediate',i.indimmediate,'nulls_not_distinct',i.indnullsnotdistinct,'clustered',i.indisclustered,'replica_identity',i.indisreplident,
 'definition',pg_catalog.pg_get_indexdef(i.indexrelid,0,false),'expressions',COALESCE(pg_catalog.pg_get_expr(i.indexprs,i.indrelid,false),''),'predicate',COALESCE(pg_catalog.pg_get_expr(i.indpred,i.indrelid,false),''),'options',COALESCE(c.reloptions,ARRAY[]::text[])) AS value
 FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid=i.indexrelid JOIN pg_catalog.pg_class t ON t.oid=i.indrelid JOIN pg_catalog.pg_namespace n ON n.oid=t.relnamespace JOIN pg_catalog.pg_am am ON am.oid=c.relam WHERE n.nspname=$1`, schema, 2048, &total)
	if err != nil {
		return fail()
	}
	catalog.Columns, err = recoverySemanticRows[RecoverySemanticColumn](ctx, tx, `SELECT pg_catalog.jsonb_build_object(
 'table',c.relname,'name',a.attname,'ordinal',a.attnum,'type_schema',tn.nspname,'type_name',t.typname,'type_modifier',a.atttypmod,'dimensions',a.attndims,'not_null',a.attnotnull,'generated',a.attgenerated::text,'identity',a.attidentity::text,
 'collation_schema',COALESCE(cn.nspname,''),'collation_name',COALESCE(cl.collname,''),'expression',COALESCE(pg_catalog.pg_get_expr(d.adbin,d.adrelid,false),'')) AS value
 FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace JOIN pg_catalog.pg_type t ON t.oid=a.atttypid JOIN pg_catalog.pg_namespace tn ON tn.oid=t.typnamespace
 LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum LEFT JOIN pg_catalog.pg_collation cl ON cl.oid=a.attcollation LEFT JOIN pg_catalog.pg_namespace cn ON cn.oid=cl.collnamespace
 WHERE n.nspname=$1 AND c.relkind IN('r','p','f','v','m') AND a.attnum>0 AND NOT a.attisdropped`, schema, 4096, &total)
	if err != nil {
		return fail()
	}
	catalog.Triggers, err = recoverySemanticRows[RecoverySemanticTrigger](ctx, tx, `SELECT pg_catalog.jsonb_build_object(
 'table',c.relname,'name',t.tgname,'type',t.tgtype,'enabled',t.tgenabled::text,'function_schema',pn.nspname,'function_name',p.proname,'function_arguments',pg_catalog.pg_get_function_identity_arguments(p.oid),'arguments_hex',pg_catalog.encode(t.tgargs,'hex'),'when',COALESCE(pg_catalog.pg_get_expr(t.tgqual,t.tgrelid,false),''),'definition',pg_catalog.pg_get_triggerdef(t.oid,false)) AS value
 FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_class c ON c.oid=t.tgrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace JOIN pg_catalog.pg_proc p ON p.oid=t.tgfoid JOIN pg_catalog.pg_namespace pn ON pn.oid=p.pronamespace WHERE n.nspname=$1 AND NOT t.tgisinternal`, schema, 2048, &total)
	if err != nil {
		return fail()
	}
	catalog.Functions, err = recoverySemanticRows[RecoverySemanticFunction](ctx, tx, `SELECT pg_catalog.jsonb_build_object(
 'name',p.proname,'arguments',pg_catalog.pg_get_function_identity_arguments(p.oid),'result',pg_catalog.pg_get_function_result(p.oid),'language',l.lanname,'kind',p.prokind::text,'security_definer',p.prosecdef,'strict',p.proisstrict,'leakproof',p.proleakproof,'returns_set',p.proretset,'volatility',p.provolatile::text,'parallel',p.proparallel::text,'cost',p.procost,'rows',p.prorows,'configuration',COALESCE(p.proconfig,ARRAY[]::text[]),'source',p.prosrc,'definition',CASE WHEN p.prokind IN('f','p') THEN pg_catalog.pg_get_functiondef(p.oid) ELSE '' END) AS value
 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace JOIN pg_catalog.pg_language l ON l.oid=p.prolang WHERE n.nspname=$1`, schema, 512, &total)
	if err != nil {
		return fail()
	}
	for i := range catalog.Constraints {
		v := &catalog.Constraints[i]
		v.RefSchema = recoverySemanticSchema(v.RefSchema, schema)
		v.Definition, err = recoverySemanticNormalizeSQL(v.Definition, schema)
		if err != nil {
			return fail()
		}
	}
	for i := range catalog.Indexes {
		v := &catalog.Indexes[i]
		for _, s := range []*string{&v.Definition, &v.Expressions, &v.Predicate} {
			*s, err = recoverySemanticNormalizeSQL(*s, schema)
			if err != nil {
				return fail()
			}
		}
		sort.Strings(v.Options)
	}
	for i := range catalog.Columns {
		v := &catalog.Columns[i]
		v.RawExpression = v.Expression
		v.Expression, err = recoverySemanticNormalizeSQL(v.Expression, schema)
		if err != nil {
			return fail()
		}
		v.TypeSchema = recoverySemanticSchema(v.TypeSchema, schema)
		v.CollationSchema = recoverySemanticSchema(v.CollationSchema, schema)
	}
	for i := range catalog.Triggers {
		v := &catalog.Triggers[i]
		v.FunctionSchema = recoverySemanticSchema(v.FunctionSchema, schema)
		for _, s := range []*string{&v.Definition, &v.When, &v.FunctionArguments} {
			*s, err = recoverySemanticNormalizeSQL(*s, schema)
			if err != nil {
				return fail()
			}
		}
	}
	for i := range catalog.Functions {
		v := &catalog.Functions[i]
		v.Definition, err = recoverySemanticNormalizeFunction(v.Definition, v.Configuration, schema)
		if err != nil {
			return fail()
		}
		for j, c := range v.Configuration {
			v.Configuration[j], err = recoverySemanticNormalizeConfig(c, schema)
			if err != nil {
				return fail()
			}
		}
		sort.Strings(v.Configuration)
	}
	for i := range catalog.Relations {
		sort.Strings(catalog.Relations[i].Options)
	}
	return catalog, nil
}

func recoverySemanticRows[T any](ctx context.Context, tx pgx.Tx, query, schema string, maxRows int, total *int) ([]T, error) {
	result := []T{}
	sql := "WITH records AS (" + query + ") SELECT CASE WHEN pg_catalog.octet_length(value::text)<=" + strconv.Itoa(recoverySemanticMaxRecord) + " THEN value::text ELSE NULL END FROM records ORDER BY COALESCE(value->>'table','') COLLATE \"C\",COALESCE(value->>'name','') COLLATE \"C\",COALESCE(value->>'arguments','') COLLATE \"C\" LIMIT " + strconv.Itoa(maxRows+1)
	rows, err := tx.Query(ctx, sql, schema)
	if err != nil {
		return nil, ErrRecoverySemanticCatalog
	}
	defer rows.Close()
	for rows.Next() {
		var raw *string
		if rows.Scan(&raw) != nil || raw == nil || len(result) >= maxRows || len(*raw) > recoverySemanticMaxBytes-*total {
			return nil, ErrRecoverySemanticCatalog
		}
		*total += len(*raw)
		var entry T
		if json.Unmarshal([]byte(*raw), &entry) != nil {
			return nil, ErrRecoverySemanticCatalog
		}
		result = append(result, entry)
	}
	if rows.Err() != nil {
		return nil, ErrRecoverySemanticCatalog
	}
	return result, nil
}
func recoverySemanticSchema(value, schema string) string {
	if value == schema {
		return recoverySemanticNamespace
	}
	return value
}

// Canonicalize only SQL identifier qualifiers outside literals, quoted bodies
// and comments. It never rewrites string contents or executable function bodies.
func recoverySemanticNormalizeSQL(input, schema string) (string, error) {
	if len(input) > recoverySemanticMaxRecord {
		return "", ErrRecoverySemanticCatalog
	}
	var out strings.Builder
	for i := 0; i < len(input); {
		start := i
		if input[i] == '\'' || input[i] == '"' {
			quote := input[i]
			escaped := quote == '\'' && i > 0 && (input[i-1] == 'e' || input[i-1] == 'E') && (i == 1 || !(input[i-2] == '_' || input[i-2] >= 'a' && input[i-2] <= 'z' || input[i-2] >= 'A' && input[i-2] <= 'Z' || input[i-2] >= '0' && input[i-2] <= '9'))
			i++
			var identifier strings.Builder
			closed := false
			for i < len(input) {
				if input[i] == quote {
					if i+1 < len(input) && input[i+1] == quote {
						identifier.WriteByte(quote)
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				if escaped && input[i] == '\\' {
					if i+1 >= len(input) {
						return "", ErrRecoverySemanticCatalog
					}
					identifier.WriteString(input[i : i+2])
					i += 2
					continue
				}
				identifier.WriteByte(input[i])
				i++
			}
			if !closed {
				return "", ErrRecoverySemanticCatalog
			}
			next := i
			for next < len(input) && unicode.IsSpace(rune(input[next])) {
				next++
			}
			if quote == '"' && identifier.String() == schema && next < len(input) && input[next] == '.' {
				out.WriteString(pgx.Identifier{recoverySemanticNamespace}.Sanitize())
			} else {
				out.WriteString(input[start:i])
			}
			continue
		}
		if input[i] == '$' {
			j := i + 1
			for j < len(input) && (input[j] == '_' || input[j] >= 'a' && input[j] <= 'z' || input[j] >= 'A' && input[j] <= 'Z' || input[j] >= '0' && input[j] <= '9') {
				j++
			}
			if j < len(input) && input[j] == '$' {
				delimiter := input[i : j+1]
				end := strings.Index(input[j+1:], delimiter)
				if end < 0 {
					return "", ErrRecoverySemanticCatalog
				}
				i = j + 1 + end + len(delimiter)
				out.WriteString(input[start:i])
				continue
			}
		}
		if strings.HasPrefix(input[i:], "--") {
			end := strings.IndexByte(input[i:], '\n')
			if end < 0 {
				out.WriteString(input[i:])
				break
			}
			i += end + 1
			out.WriteString(input[start:i])
			continue
		}
		if strings.HasPrefix(input[i:], "/*") {
			depth := 1
			i += 2
			for i < len(input) && depth > 0 {
				if strings.HasPrefix(input[i:], "/*") {
					depth++
					i += 2
				} else if strings.HasPrefix(input[i:], "*/") {
					depth--
					i += 2
				} else {
					i++
				}
			}
			if depth != 0 {
				return "", ErrRecoverySemanticCatalog
			}
			out.WriteString(input[start:i])
			continue
		}
		if input[i] == '_' || input[i] >= 'a' && input[i] <= 'z' || input[i] >= 'A' && input[i] <= 'Z' {
			i++
			for i < len(input) && (input[i] == '_' || input[i] == '$' || input[i] >= 'a' && input[i] <= 'z' || input[i] >= 'A' && input[i] <= 'Z' || input[i] >= '0' && input[i] <= '9') {
				i++
			}
			next := i
			for next < len(input) && unicode.IsSpace(rune(input[next])) {
				next++
			}
			if strings.ToLower(input[start:i]) == schema && next < len(input) && input[next] == '.' {
				out.WriteString(pgx.Identifier{recoverySemanticNamespace}.Sanitize())
			} else {
				out.WriteString(input[start:i])
			}
			continue
		}
		out.WriteByte(input[i])
		i++
	}
	return out.String(), nil
}
func recoverySemanticNormalizeConfig(value, schema string) (string, error) {
	if !strings.HasPrefix(value, "search_path=") {
		return value, nil
	}
	// Only SQL022's explicit schema pin is supported; no ambiguous search_path
	// parsing, '$user' expansion, duplicate namespace, or unexpected override.
	expected := schema + ", pg_catalog, pg_temp"
	quoted := pgx.Identifier{schema}.Sanitize() + ", pg_catalog, pg_temp"
	raw := strings.TrimPrefix(value, "search_path=")
	if raw != expected && raw != quoted {
		return "", ErrRecoverySemanticCatalog
	}
	return "search_path=" + recoverySemanticNamespace + ", pg_catalog, pg_temp", nil
}
func recoverySemanticNormalizeFunction(definition string, configuration []string, schema string) (string, error) {
	// pg_get_functiondef's AS dollar body follows its header/config clauses.
	// Preserve the body verbatim and normalize only the deparsed header config.
	body := strings.Index(definition, "\nAS $")
	if body < 0 {
		return "", ErrRecoverySemanticCatalog
	}
	prefix, suffix := definition[:body], definition[body:]
	for _, cfg := range configuration {
		if strings.HasPrefix(cfg, "search_path=") {
			normalized, err := recoverySemanticNormalizeConfig(cfg, schema)
			if err != nil {
				return "", err
			}
			found := false
			lines := strings.Split(prefix, "\n")
			for i, line := range lines {
				if strings.HasPrefix(strings.TrimSpace(line), "SET search_path TO ") {
					if found {
						return "", ErrRecoverySemanticCatalog
					}
					lines[i] = " SET " + normalized
					found = true
				}
			}
			if !found {
				return "", ErrRecoverySemanticCatalog
			}
			prefix = strings.Join(lines, "\n")
		}
	}
	normalized, err := recoverySemanticNormalizeSQL(prefix, schema)
	if err != nil {
		return "", err
	}
	if len(normalized)+len(suffix) > recoverySemanticMaxRecord {
		return "", ErrRecoverySemanticCatalog
	}
	return normalized + suffix, nil
}

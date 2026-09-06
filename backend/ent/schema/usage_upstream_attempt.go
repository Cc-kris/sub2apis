package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/shopspring/decimal"
)

// UsageUpstreamAttempt stores immutable billing facts for each routed upstream attempt.
type UsageUpstreamAttempt struct {
	ent.Schema
}

func (UsageUpstreamAttempt) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "usage_upstream_attempts"}}
}

func (UsageUpstreamAttempt) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("usage_log_id"),
		field.String("request_id").MaxLen(64),
		field.Int("attempt_no").Positive(),
		field.Int64("account_id"),
		field.Int64("channel_id").Optional().Nillable(),
		field.String("upstream_model").MaxLen(100),
		field.String("service_tier").MaxLen(50).Optional().Nillable(),
		field.Int64("input_tokens").NonNegative().Default(0),
		field.Int64("output_tokens").NonNegative().Default(0),
		field.Int64("cache_read_tokens").NonNegative().Default(0),
		field.Int64("cache_creation_tokens").NonNegative().Default(0),
		field.Int64("cache_creation_5m_tokens").NonNegative().Default(0),
		field.Int64("cache_creation_1h_tokens").NonNegative().Default(0),
		field.Int64("request_count").NonNegative().Default(0),
		field.Int64("image_count").NonNegative().Default(0),
		field.Int64("video_seconds").NonNegative().Default(0),
		field.Float("upstream_cost_multiplier").
			GoType(decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "decimal(10,4)"}).
			Optional().
			Nillable(),
		field.Int64("upstream_multiplier_change_id").Optional().Nillable(),
		field.String("upstream_multiplier_source").MaxLen(30).Optional().Nillable(),
		field.Time("upstream_multiplier_effective_at").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Int64("account_finance_profile_id").Optional().Nillable(),
		field.Bool("billable").Default(false),
		field.Time("billing_observed_at").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Float("upstream_actual_charge").GoType(decimal.Decimal{}).SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}).Optional().Nillable(),
		field.Float("upstream_actual_charge_usd").GoType(decimal.Decimal{}).SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}).Optional().Nillable(),
		field.Float("upstream_standard_charge").GoType(decimal.Decimal{}).SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}).Optional().Nillable(),
		field.String("upstream_charge_currency").MaxLen(30).Optional().Nillable(),
		field.String("upstream_charge_unit_semantics").MaxLen(30).Optional().Nillable(),
		field.String("upstream_billing_request_id").MaxLen(200).Optional().Nillable(),
		field.JSON("upstream_charge_snapshot", map[string]any{}).Optional(),
		field.Time("completed_at").Default(time.Now).SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("created_at").Default(time.Now).Immutable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (UsageUpstreamAttempt) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("usage_log_id", "attempt_no").Unique(),
		index.Fields("request_id", "attempt_no"),
		index.Fields("account_id", "created_at"),
	}
}

package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// StatDay é uma linha da série temporal diária (dia = calendário São Paulo,
// mesma convenção do período de billing). Envelope compartilhado com a Editora.
type StatDay struct {
	Day       string `json:"day"` // AAAA-MM-DD (America/Sao_Paulo)
	Respostas int    `json:"respostas"`
	Conversas int    `json:"conversas"`
	Handoffs  int    `json:"handoffs"`
}

// spDay converte created_at para o dia-calendário de São Paulo como AAAA-MM-DD.
const spDay = `to_char(created_at AT TIME ZONE 'America/Sao_Paulo', 'YYYY-MM-DD')`
const spMonth = `to_char(created_at AT TIME ZONE 'America/Sao_Paulo', 'YYYY-MM')`

// DailyStats agrega, por dia São Paulo do período, as respostas da IA (out/bot),
// as conversas novas e os handoffs. Tenant-scoped (roda sob WithTenant). As três
// tabelas são agregadas em separado e mescladas por dia.
func DailyStats(ctx context.Context, tx pgx.Tx, period string) ([]StatDay, error) {
	byDay := map[string]*StatDay{}
	get := func(day string) *StatDay {
		if d, ok := byDay[day]; ok {
			return d
		}
		d := &StatDay{Day: day}
		byDay[day] = d
		return d
	}

	// respostas da IA
	if err := scanCounts(ctx, tx,
		`SELECT `+spDay+` AS day, count(*)::int AS n FROM messages
		  WHERE direction='out' AND sender='bot' AND `+spMonth+` = $1 GROUP BY day`,
		period, func(day string, n int) { get(day).Respostas = n }); err != nil {
		return nil, err
	}
	// conversas novas
	if err := scanCounts(ctx, tx,
		`SELECT `+spDay+` AS day, count(*)::int AS n FROM conversations
		  WHERE `+spMonth+` = $1 GROUP BY day`,
		period, func(day string, n int) { get(day).Conversas = n }); err != nil {
		return nil, err
	}
	// handoffs
	if err := scanCounts(ctx, tx,
		`SELECT `+spDay+` AS day, count(*)::int AS n FROM handoffs
		  WHERE `+spMonth+` = $1 GROUP BY day`,
		period, func(day string, n int) { get(day).Handoffs = n }); err != nil {
		return nil, err
	}

	// ordena por dia (AAAA-MM-DD ordena lexicograficamente = cronológico)
	days := make([]string, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	sortStrings(days)
	out := make([]StatDay, 0, len(days))
	for _, d := range days {
		out = append(out, *byDay[d])
	}
	return out, nil
}

func scanCounts(ctx context.Context, tx pgx.Tx, q, period string, set func(day string, n int)) error {
	rows, err := tx.Query(ctx, q, period)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var day string
		var n int
		if err := rows.Scan(&day, &n); err != nil {
			return err
		}
		set(day, n)
	}
	return rows.Err()
}

// sortStrings ordena in-place (evita importar sort só p/ isso e manter o arquivo enxuto).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

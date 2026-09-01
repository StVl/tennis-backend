// Package livedb отдаёт DDL live-ingest'а в виде встроенных в бинарь файлов.
//
// Пакет лежит В КАТАЛОГЕ db/ намеренно: go:embed не умеет пути с "..", а
// переносить SQL к коду нельзя — эти же файлы переносятся в схему
// tennis-data-storage и на них ссылается документация.
//
// dev_fixtures.sql здесь НЕТ и быть не должно: он создаёт синтетические матчи
// для локальной отладки, а этот набор применяется в том числе на проде.
package livedb

import "embed"

//go:embed live_ingest.sql live_push.sql live_external_ids.sql live_edition_ids.sql
var files embed.FS

// File — один шаг применения схемы.
type File struct {
	Name string
	SQL  string
}

// Files возвращает файлы В ПОРЯДКЕ ПРИМЕНЕНИЯ. Порядок значим: сиды джойнятся
// по слагам из players и tournament_editions, поэтому идут после DDL, а на
// пустой базе просто не находят строк — это нормально и не ошибка.
func Files() ([]File, error) {
	names := []string{
		"live_ingest.sql",
		"live_push.sql",
		"live_external_ids.sql",
		"live_edition_ids.sql",
	}
	out := make([]File, 0, len(names))
	for _, n := range names {
		b, err := files.ReadFile(n)
		if err != nil {
			return nil, err
		}
		out = append(out, File{Name: n, SQL: string(b)})
	}
	return out, nil
}

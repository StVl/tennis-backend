package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
)

func NewRouter(pool *pgxpool.Pool) http.Handler {
	handler := NewHandler(pool)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/hello", handler.Hello)
	r.Get("/health", handler.Health)

	r.Route("/v1", func(r chi.Router) {
		// игроки
		r.Get("/players", handler.ListPlayers)
		r.Get("/players/{slug}", handler.GetPlayer)
		r.Get("/players/{slug}/matches", handler.ListPlayerMatches)
		r.Get("/players/{slug}/tournaments", handler.ListPlayerTournaments)
		r.Get("/players/{slug}/ranking-history", handler.PlayerRankingHistory)
		r.Get("/players/{slug}/h2h/{other}", handler.HeadToHead)

		// турниры и рейтинг
		r.Get("/tournaments", handler.ListTournaments)
		r.Get("/tournaments/{slug}", handler.GetTournament)
		r.Get("/tournaments/{slug}/draw", handler.TournamentDraw)
		r.Get("/tournaments/{slug}/history", handler.TournamentHistory)
		r.Get("/rankings", handler.ListRankings)
		r.Get("/play-styles", handler.ListPlayStyles)

		// матчи
		r.Get("/matches", handler.ListMatches)
		r.Get("/matches/{id}", handler.GetMatch)
	})

	return r
}

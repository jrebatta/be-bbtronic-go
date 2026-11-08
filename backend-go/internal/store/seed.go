package store

import (
	"context"
	"fmt"
)

// Seed data copied from WaitingMessageSeeder and ImpostorWordSeeder. They run
// only through the admin tool and only into empty tables (never at start-up,
// never duplicating already loaded catalogs).

// WaitingTemplates are the three Java waiting messages.
var WaitingTemplates = []string{
	"Hasta que hora te esperamos {usuarios}....",
	"Tengo toda la noche {usuarios}...",
	"{usuarios} avisan",
}

// ImpostorWordSeed lists the 150 Java words by category (25 each).
var ImpostorWordSeed = []struct {
	Category string
	Words    []string
}{
	{"animales", []string{"gato", "perro", "elefante", "tigre", "jirafa", "cocodrilo", "delfín",
		"loro", "pingüino", "hormiga", "lobo", "zorro", "caballo", "panda",
		"koala", "iguana", "cangrejo", "medusa", "pulpo", "murciélago",
		"ballena", "flamenco", "avestruz", "rinoceronte", "chimpancé"}},
	{"comida", []string{"pizza", "hamburguesa", "sushi", "tacos", "pasta", "ensalada",
		"chocolate", "helado", "pollo", "sandía", "mango", "sopa", "ceviche",
		"empanada", "arepa", "paella", "ramen", "burrito", "waffles", "churros",
		"tiramisu", "croquetas", "gazpacho", "tamales", "brigadeiro"}},
	{"deportes", []string{"fútbol", "baloncesto", "tenis", "natación", "ciclismo", "boxeo",
		"esquí", "voleibol", "golf", "béisbol", "surf", "judo", "escalada",
		"patinaje", "ajedrez", "polo", "rugby", "kárate", "triatlón", "esgrima",
		"curling", "críquet", "lacrosse", "halterofilia", "pentatlón"}},
	{"objetos", []string{"tijeras", "martillo", "paraguas", "linterna", "brújula", "termómetro",
		"calculadora", "microscopio", "telescopio", "lupa", "cerradura", "cojín",
		"espejo", "botella", "mochila", "ventilador", "maletín", "candado",
		"auriculares", "perchero", "sacacorchos", "abrelatas", "embudo",
		"bisturí", "compás"}},
	{"lugares", []string{"aeropuerto", "mercado", "hospital", "estadio", "biblioteca", "zoológico",
		"faro", "monasterio", "volcán", "cueva", "desierto", "isla", "glaciar",
		"catedral", "acuario", "casino", "observatorio", "granja", "puerto",
		"cementerio", "laberinto", "catacumba", "invernadero", "embajada", "fiordo"}},
	{"peliculas", []string{"Titanic", "Matrix", "Inception", "Shrek", "Coco", "Rocky", "Alien",
		"Gladiator", "Joker", "Interstellar", "Frozen", "Moana", "Encanto",
		"Parasite", "Avatar", "Jaws", "Psycho", "Grease", "Braveheart",
		"Scarface", "Beetlejuice", "Casablanca", "Spotlight", "Boyhood", "Arrival"}},
}

// SeedResult reports what the seeder inserted.
type SeedResult struct {
	WaitingInserted int
	WordsInserted   int
}

// SeedCatalogs inserts the Java seeds into empty tables only.
func (s *Store) SeedCatalogs(ctx context.Context) (SeedResult, error) {
	var res SeedResult
	err := s.InTx(ctx, func(tx *Tx) error {
		if _, err := tx.tx.Exec(ctx, `LOCK TABLE waiting_messages, impostor_word IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return fmt.Errorf("locking seed tables: %w", err)
		}
		var n int
		if err := tx.tx.QueryRow(ctx, `SELECT count(*) FROM waiting_messages`).Scan(&n); err != nil {
			return fmt.Errorf("counting waiting messages: %w", err)
		}
		if n == 0 {
			for _, t := range WaitingTemplates {
				if _, err := tx.tx.Exec(ctx, `INSERT INTO waiting_messages (template) VALUES ($1)`, t); err != nil {
					return fmt.Errorf("seeding waiting messages: %w", err)
				}
				res.WaitingInserted++
			}
		}
		if err := tx.tx.QueryRow(ctx, `SELECT count(*) FROM impostor_word`).Scan(&n); err != nil {
			return fmt.Errorf("counting impostor words: %w", err)
		}
		if n == 0 {
			for _, c := range ImpostorWordSeed {
				for _, w := range c.Words {
					if _, err := tx.tx.Exec(ctx, `INSERT INTO impostor_word (word, category) VALUES ($1, $2)`, w, c.Category); err != nil {
						return fmt.Errorf("seeding impostor words: %w", err)
					}
					res.WordsInserted++
				}
			}
		}
		return nil
	})
	return res, err
}

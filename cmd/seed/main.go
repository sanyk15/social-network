// Command seed fills the users table with profiles from a CSV file.
//
// The CSV rows have the form "Фамилия Имя,YYYY-MM-DD,Город" and contain no
// header. Rows are shuffled before insertion so that id does not correlate
// with the name, which would make index benchmarks unrealistically good.
package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"social-network/internal/config"
	"social-network/internal/store"
)

type person struct {
	firstName string
	lastName  string
	birthDate time.Time
	city      string
	gender    string
}

var interestsPool = []string{
	"music", "travel", "sport", "reading", "cinema", "cooking",
	"photography", "gaming", "art", "technology", "fashion", "fitness",
	"history", "science", "nature", "dancing", "theatre", "cars",
}

func main() {
	csvPath := flag.String("csv", "people.v2.csv", "path to CSV: '<last> <first>,YYYY-MM-DD,<city>'")
	dsn := flag.String("dsn", "", "Postgres DSN (default: DATABASE_URL or the local default)")
	count := flag.Int("n", 1_000_000, "number of profiles to insert")
	batchSize := flag.Int("batch", 50_000, "rows per COPY batch")
	seed := flag.Int64("seed", 42, "shuffle and random seed")
	truncate := flag.Bool("truncate", false, "truncate users and sessions before seeding")
	password := flag.String("password", "password123", "password given to every seeded profile")
	flag.Parse()

	if *count <= 0 {
		log.Fatal("n must be positive")
	}
	if *batchSize <= 0 {
		log.Fatal("batch must be positive")
	}

	ctx := context.Background()
	cfg := config.Load()
	if *dsn == "" {
		*dsn = cfg.DatabaseURL
	}

	people, err := loadPeople(*csvPath)
	if err != nil {
		log.Fatalf("load %s: %v", *csvPath, err)
	}

	rnd := rand.New(rand.NewSource(*seed))
	rnd.Shuffle(len(people), func(i, j int) {
		people[i], people[j] = people[j], people[i]
	})

	st, err := store.New(ctx, *dsn, cfg.DBMaxConns)
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	if *truncate {
		if err := st.TruncateUsers(ctx); err != nil {
			log.Fatalf("truncate: %v", err)
		}
		log.Print("truncated users and sessions")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(*password), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("hash password: %v", err)
	}

	start := time.Now()
	inserted := 0
	for from := 0; from < *count; from += *batchSize {
		to := min(from+*batchSize, *count)
		batch := make([]store.UserSeed, 0, to-from)

		for i := from; i < to; i++ {
			p := people[i%len(people)]

			nInterests := 1 + rnd.Intn(3)
			interests := make([]string, nInterests)
			for k := range interests {
				interests[k] = interestsPool[rnd.Intn(len(interestsPool))]
			}

			batch = append(batch, store.UserSeed{
				Username:     fmt.Sprintf("user%07d", i+1),
				PasswordHash: string(hash),
				FirstName:    p.firstName,
				LastName:     p.lastName,
				BirthDate:    p.birthDate,
				Gender:       p.gender,
				Interests:    interests,
				City:         p.city,
			})
		}

		if err := st.InsertUsersBatch(ctx, batch); err != nil {
			log.Fatalf("insert batch %d-%d: %v", from, to-1, err)
		}
		inserted += len(batch)

		rate := float64(inserted) / time.Since(start).Seconds()
		log.Printf("inserted %d/%d (%.0f rows/s)", inserted, *count, rate)
	}

	log.Print("analyzing users")
	if err := st.AnalyzeUsers(ctx); err != nil {
		log.Fatalf("analyze: %v", err)
	}

	log.Printf("done: %d profiles in %s (password %q, usernames user%%07d)",
		inserted, time.Since(start).Round(time.Millisecond), *password)
}

func loadPeople(path string) ([]person, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = 3
	r.ReuseRecord = false

	people := make([]person, 0, 1_000_000)
	skipped := 0
	for line := 1; ; line++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}

		fio, rawBirthDate, city := rec[0], rec[1], rec[2]
		if !utf8.ValidString(fio) || !utf8.ValidString(city) {
			skipped++
			continue
		}

		parts := strings.SplitN(strings.TrimSpace(fio), " ", 2)
		if len(parts) != 2 {
			skipped++
			continue
		}
		lastName, firstName := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if lastName == "" || firstName == "" {
			skipped++
			continue
		}

		birthDate, err := time.Parse("2006-01-02", rawBirthDate)
		if err != nil {
			skipped++
			continue
		}

		gender := "male"
		if strings.HasSuffix(lastName, "а") || strings.HasSuffix(lastName, "я") {
			gender = "female"
		}

		people = append(people, person{
			firstName: firstName,
			lastName:  lastName,
			birthDate: birthDate,
			city:      city,
			gender:    gender,
		})
	}

	if len(people) == 0 {
		return nil, errors.New("no valid rows")
	}
	log.Printf("loaded %d profiles from %s (skipped %d)", len(people), path, skipped)
	return people, nil
}

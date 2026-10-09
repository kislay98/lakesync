// Package seed generates realistic, messy test data in MongoDB (brief 13.1).
package seed

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Options configure a seed run.
type Options struct {
	URI               string
	Database          string
	Initial           map[string]int // documents per collection
	Rate              float64        // operations per second during the live phase
	Duration          time.Duration  // live phase length; 0 = only initial load
	NewFieldAfter     time.Duration  // introduce a brand-new field after this
	TypeConflictAfter time.Duration  // introduce type conflicts after this
	Mix               Mix
	Seed              int64
	Quiet             bool
	Drop              bool // drop the database first
}

// Mix is the operation mix in percent.
type Mix struct{ Insert, Update, Replace, Delete int }

// Main is the CLI entry point.
func Main(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	uri := fs.String("uri", envOr("SEED_MONGODB_URI", "mongodb://localhost:27017/?directConnection=true"), "MongoDB URI")
	db := fs.String("db", "shop", "database name")
	initial := fs.String("initial", "customers=2000,products=500,orders=5000,events=10000", "initial document counts")
	rate := fs.Float64("rate", 50, "operations per second (keep under 100 for Atlas Free)")
	duration := fs.Duration("duration", 0, "how long to generate live changes (0 = initial load only)")
	newField := fs.Duration("new-field-after", 0, "introduce a brand-new field after this duration (0 = never)")
	conflict := fs.Duration("type-conflict-after", 0, "introduce type conflicts after this duration (0 = never)")
	mix := fs.String("mix", "insert=50,update=30,replace=10,delete=10", "operation mix in percent")
	seedV := fs.Int64("seed", time.Now().UnixNano(), "random seed")
	drop := fs.Bool("drop", false, "drop the database before loading")
	quiet := fs.Bool("quiet", false, "less output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts := Options{URI: *uri, Database: *db, Initial: map[string]int{}, Rate: *rate, Duration: *duration, NewFieldAfter: *newField, TypeConflictAfter: *conflict, Seed: *seedV, Quiet: *quiet, Drop: *drop}
	for _, kv := range strings.Split(*initial, ",") {
		parts := strings.SplitN(strings.TrimSpace(kv), "=", 2)
		if len(parts) == 2 {
			var v int
			fmt.Sscan(parts[1], &v)
			opts.Initial[strings.TrimSpace(parts[0])] = v
		}
	}
	for _, kv := range strings.Split(*mix, ",") {
		parts := strings.SplitN(strings.TrimSpace(kv), "=", 2)
		if len(parts) != 2 {
			continue
		}
		var v int
		fmt.Sscan(parts[1], &v)
		switch parts[0] {
		case "insert":
			opts.Mix.Insert = v
		case "update":
			opts.Mix.Update = v
		case "replace":
			opts.Mix.Replace = v
		case "delete":
			opts.Mix.Delete = v
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return Run(ctx, opts)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

var (
	firstNames = []string{"Aarav", "Zoë", "Mateo", "Ngozi", "Søren", "Yuki", "Fatima", "Liam", "Priya", "Jürgen", "Chloé", "Kwame", "Ingrid", "Rafael", "Aoife", "Dmitri", "Mei", "Olumide", "Anaïs", "Björn"}
	lastNames  = []string{"García", "Müller", "Okafor", "Nakamura", "O'Brien", "Smith", "Rossi", "Kowalski", "Haddad", "Nguyen", "Andersson", "Silva", "Dubois", "Ivanova", "Patel", "Yılmaz", "Chen", "Mensah", "Novák", "Larsen"}
	cities     = []string{"Lisbon", "Nairobi", "Osaka", "Montréal", "São Paulo", "Zürich", "Mumbai", "Reykjavík", "Austin", "Kraków"}
	countries  = []string{"PT", "KE", "JP", "CA", "BR", "CH", "IN", "IS", "US", "PL"}
	tags       = []string{"vip", "newsletter", "wholesale", "churn-risk", "beta", "returning", "mobile"}
	statuses   = []string{"pending", "paid", "shipped", "delivered", "cancelled", "refunded"}
	eventTypes = []string{"page_view", "add_to_cart", "checkout", "search", "login", "logout"}
	categories = []string{"shoes", "bags", "hats", "jackets", "watches"}
)

type state struct {
	opts      Options
	rng       *rand.Rand
	db        *mongo.Database
	ids       map[string][]bson.ObjectID // live ids per collection
	customers []bson.ObjectID
	products  []bson.ObjectID
	start     time.Time
	counts    map[string]int
}

// Run executes the initial load and the live phase.
func Run(ctx context.Context, opts Options) error {
	if opts.Mix.Insert+opts.Mix.Update+opts.Mix.Replace+opts.Mix.Delete == 0 {
		opts.Mix = Mix{50, 30, 10, 10}
	}
	if opts.Database == "" {
		opts.Database = "shop"
	}
	client, err := mongo.Connect(options.Client().ApplyURI(opts.URI).SetAppName("lakesync-seed").SetTimeout(60 * time.Second))
	if err != nil {
		return err
	}
	defer client.Disconnect(context.Background())
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := client.Ping(pctx, nil); err != nil {
		return fmt.Errorf("cannot reach MongoDB: %w", err)
	}
	s := &state{opts: opts, rng: rand.New(rand.NewSource(opts.Seed)), db: client.Database(opts.Database), ids: map[string][]bson.ObjectID{}, counts: map[string]int{}}
	if opts.Drop {
		if err := s.db.Drop(ctx); err != nil {
			return err
		}
	}
	if err := s.initialLoad(ctx); err != nil {
		return err
	}
	if opts.Duration <= 0 {
		s.summary()
		return nil
	}
	return s.live(ctx)
}

func (s *state) logf(format string, a ...any) {
	if !s.opts.Quiet {
		fmt.Printf(format+"\n", a...)
	}
}

func (s *state) initialLoad(ctx context.Context) error {
	s.start = time.Now()
	order := []string{"customers", "products", "orders", "events"}
	for _, coll := range order {
		n := s.opts.Initial[coll]
		if n <= 0 {
			continue
		}
		s.logf("loading %d %s ...", n, coll)
		const chunk = 1000
		for done := 0; done < n; done += chunk {
			m := min(chunk, n-done)
			docs := make([]any, 0, m)
			for i := 0; i < m; i++ {
				docs = append(docs, s.newDoc(coll))
			}
			res, err := s.db.Collection(coll).InsertMany(ctx, docs)
			if err != nil {
				return fmt.Errorf("insert %s: %w", coll, err)
			}
			for _, id := range res.InsertedIDs {
				if oid, ok := id.(bson.ObjectID); ok {
					s.ids[coll] = append(s.ids[coll], oid)
				}
			}
			s.counts["insert"] += m
		}
	}
	s.customers = s.ids["customers"]
	s.products = s.ids["products"]
	s.logf("initial load done in %s", time.Since(s.start).Round(time.Millisecond))
	return nil
}

func (s *state) pick(coll string) (bson.ObjectID, bool) {
	ids := s.ids[coll]
	if len(ids) == 0 {
		return bson.ObjectID{}, false
	}
	return ids[s.rng.Intn(len(ids))], true
}

func (s *state) remove(coll string, id bson.ObjectID) {
	ids := s.ids[coll]
	for i, x := range ids {
		if x == id {
			ids[i] = ids[len(ids)-1]
			s.ids[coll] = ids[:len(ids)-1]
			return
		}
	}
}

func (s *state) phase(after time.Duration) bool {
	return after > 0 && time.Since(s.start) >= after
}

func (s *state) newDoc(coll string) bson.D {
	newField := s.phase(s.opts.NewFieldAfter)
	switch coll {
	case "customers":
		d := bson.D{
			{Key: "name", Value: firstNames[s.rng.Intn(len(firstNames))] + " " + lastNames[s.rng.Intn(len(lastNames))]},
			{Key: "email", Value: fmt.Sprintf("user%d@example.com", s.rng.Intn(1_000_000))},
			{Key: "address", Value: bson.D{{Key: "street", Value: fmt.Sprintf("%d Main St", s.rng.Intn(999))}, {Key: "city", Value: cities[s.rng.Intn(len(cities))]}, {Key: "country", Value: countries[s.rng.Intn(len(countries))]}}},
			{Key: "tags", Value: pickTags(s.rng)},
			{Key: "createdAt", Value: time.Now().Add(-time.Duration(s.rng.Intn(365*24)) * time.Hour)},
			{Key: "active", Value: s.rng.Intn(10) > 1},
		}
		// phone: sometimes a string, sometimes a number
		if s.rng.Intn(3) == 0 {
			d = append(d, bson.E{Key: "phone", Value: int64(2000000000 + s.rng.Intn(999999999))})
		} else {
			d = append(d, bson.E{Key: "phone", Value: fmt.Sprintf("+1-555-%04d", s.rng.Intn(10000))})
		}
		// optional fields in some documents only
		if s.rng.Intn(2) == 0 {
			d = append(d, bson.E{Key: "age", Value: int32(18 + s.rng.Intn(60))})
		}
		if s.rng.Intn(4) == 0 {
			d = append(d, bson.E{Key: "notes", Value: "Prefers ☕ over 🍵"})
		}
		if s.rng.Intn(5) == 0 {
			d = append(d, bson.E{Key: "lastLogin", Value: nil})
		}
		if newField {
			d = append(d, bson.E{Key: "loyaltyTier", Value: []string{"bronze", "silver", "gold"}[s.rng.Intn(3)]})
		}
		return d
	case "products":
		price, _ := bson.ParseDecimal128(fmt.Sprintf("%d.%02d", 5+s.rng.Intn(500), s.rng.Intn(100)))
		attrs := bson.D{{Key: "color", Value: []string{"red", "blue", "black", "white"}[s.rng.Intn(4)]}}
		if s.rng.Intn(2) == 0 {
			attrs = append(attrs, bson.E{Key: "size", Value: []string{"S", "M", "L", "XL"}[s.rng.Intn(4)]})
		}
		if s.rng.Intn(3) == 0 {
			attrs = append(attrs, bson.E{Key: "material", Value: "leather"})
		}
		d := bson.D{
			{Key: "sku", Value: fmt.Sprintf("SKU-%06d", s.rng.Intn(1_000_000))},
			{Key: "name", Value: titleCase(categories[s.rng.Intn(len(categories))]) + " " + fmt.Sprint(s.rng.Intn(1000))},
			{Key: "category", Value: categories[s.rng.Intn(len(categories))]},
			{Key: "price", Value: price},
			{Key: "attributes", Value: attrs},
			{Key: "images", Value: bson.A{fmt.Sprintf("https://img.example.com/%d.jpg", s.rng.Intn(99999)), fmt.Sprintf("https://img.example.com/%d.jpg", s.rng.Intn(99999))}},
			{Key: "stock", Value: int32(s.rng.Intn(500))},
		}
		return d
	case "orders":
		n := 1 + s.rng.Intn(4)
		items := bson.A{}
		var total float64
		for i := 0; i < n; i++ {
			pid, _ := s.pick("products")
			qty := 1 + s.rng.Intn(3)
			unit := float64(5+s.rng.Intn(200)) + float64(s.rng.Intn(100))/100
			total += unit * float64(qty)
			items = append(items, bson.D{{Key: "productId", Value: pid}, {Key: "qty", Value: int32(qty)}, {Key: "unitPrice", Value: unit}})
		}
		cid, _ := s.pick("customers")
		d := bson.D{
			{Key: "customerId", Value: cid},
			{Key: "items", Value: items},
			{Key: "status", Value: statuses[s.rng.Intn(len(statuses))]},
			{Key: "createdAt", Value: time.Now().Add(-time.Duration(s.rng.Intn(90*24)) * time.Hour)},
		}
		// total: sometimes an int, sometimes a double
		if s.rng.Intn(2) == 0 {
			d = append(d, bson.E{Key: "total", Value: int64(total)})
		} else {
			d = append(d, bson.E{Key: "total", Value: total})
		}
		if newField {
			d = append(d, bson.E{Key: "warehouse", Value: []string{"east", "west", "north"}[s.rng.Intn(3)]})
		}
		return d
	default: // events
		cid, _ := s.pick("customers")
		return bson.D{
			{Key: "type", Value: eventTypes[s.rng.Intn(len(eventTypes))]},
			{Key: "customerId", Value: cid},
			{Key: "at", Value: time.Now()},
			{Key: "value", Value: s.rng.Intn(1000)},
		}
	}
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func pickTags(rng *rand.Rand) bson.A {
	n := rng.Intn(4)
	out := bson.A{}
	for i := 0; i < n; i++ {
		out = append(out, tags[rng.Intn(len(tags))])
	}
	return out
}

// weights for picking a collection during the live phase
var liveWeights = []struct {
	coll string
	w    int
}{{"events", 50}, {"orders", 25}, {"customers", 15}, {"products", 10}}

func (s *state) pickCollection() string {
	r := s.rng.Intn(100)
	acc := 0
	for _, lw := range liveWeights {
		acc += lw.w
		if r < acc {
			return lw.coll
		}
	}
	return "events"
}

func (s *state) live(ctx context.Context) error {
	s.logf("live phase: %.0f ops/s for %s (mix insert=%d update=%d replace=%d delete=%d)", s.opts.Rate, s.opts.Duration, s.opts.Mix.Insert, s.opts.Mix.Update, s.opts.Mix.Replace, s.opts.Mix.Delete)
	deadline := time.After(s.opts.Duration)
	interval := time.Duration(float64(time.Second) / s.opts.Rate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	report := time.NewTicker(30 * time.Second)
	defer report.Stop()
	for {
		select {
		case <-ctx.Done():
			s.summary()
			return nil
		case <-deadline:
			s.summary()
			return nil
		case <-report.C:
			s.logf("... %s elapsed: %v", time.Since(s.start).Round(time.Second), s.counts)
		case <-ticker.C:
			if err := s.oneOp(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				s.logf("op failed (continuing): %v", err)
				time.Sleep(500 * time.Millisecond)
			}
		}
	}
}

func (s *state) oneOp(ctx context.Context) error {
	coll := s.pickCollection()
	c := s.db.Collection(coll)
	r := s.rng.Intn(100)
	m := s.opts.Mix
	octx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	switch {
	case r < m.Insert:
		res, err := c.InsertOne(octx, s.newDoc(coll))
		if err != nil {
			return err
		}
		if oid, ok := res.InsertedID.(bson.ObjectID); ok {
			s.ids[coll] = append(s.ids[coll], oid)
		}
		s.counts["insert"]++
	case r < m.Insert+m.Update:
		id, ok := s.pick(coll)
		if !ok {
			return nil
		}
		_, err := c.UpdateByID(octx, id, bson.D{{Key: "$set", Value: s.updateFields(coll)}})
		if err != nil {
			return err
		}
		s.counts["update"]++
	case r < m.Insert+m.Update+m.Replace:
		id, ok := s.pick(coll)
		if !ok {
			return nil
		}
		_, err := c.ReplaceOne(octx, bson.D{{Key: "_id", Value: id}}, s.newDoc(coll))
		if err != nil {
			return err
		}
		s.counts["replace"]++
	default:
		id, ok := s.pick(coll)
		if !ok {
			return nil
		}
		if _, err := c.DeleteOne(octx, bson.D{{Key: "_id", Value: id}}); err != nil {
			return err
		}
		s.remove(coll, id)
		s.counts["delete"]++
	}
	return nil
}

func (s *state) updateFields(coll string) bson.D {
	conflict := s.phase(s.opts.TypeConflictAfter)
	newField := s.phase(s.opts.NewFieldAfter)
	switch coll {
	case "customers":
		d := bson.D{{Key: "active", Value: s.rng.Intn(2) == 0}, {Key: "tags", Value: pickTags(s.rng)}}
		if conflict && s.rng.Intn(3) == 0 {
			d = append(d, bson.E{Key: "age", Value: "unknown"}) // string into a long column
		} else {
			d = append(d, bson.E{Key: "age", Value: int32(18 + s.rng.Intn(60))})
		}
		if newField {
			d = append(d, bson.E{Key: "loyaltyTier", Value: []string{"bronze", "silver", "gold"}[s.rng.Intn(3)]})
		}
		return d
	case "products":
		price, _ := bson.ParseDecimal128(fmt.Sprintf("%d.%02d", 5+s.rng.Intn(500), s.rng.Intn(100)))
		var stock any = int32(s.rng.Intn(500))
		if conflict && s.rng.Intn(4) == 0 {
			stock = "out of stock" // string into a long column
		}
		d := bson.D{{Key: "price", Value: price}, {Key: "stock", Value: stock}}
		return d
	case "orders":
		d := bson.D{{Key: "status", Value: statuses[s.rng.Intn(len(statuses))]}}
		if conflict && s.rng.Intn(3) == 0 {
			d = append(d, bson.E{Key: "total", Value: "n/a"})
		}
		if newField {
			d = append(d, bson.E{Key: "warehouse", Value: []string{"east", "west", "north"}[s.rng.Intn(3)]})
		}
		return d
	default:
		return bson.D{{Key: "value", Value: s.rng.Intn(1000)}}
	}
}

func (s *state) summary() {
	s.logf("done: %v in %s", s.counts, time.Since(s.start).Round(time.Second))
}

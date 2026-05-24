package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/glebarez/sqlite"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
)

const SchemaVersion = 1

type IDModel struct {
	BuildTimestamp string `gorm:"column:build_timestamp"`
	SchemaVersion  int    `gorm:"column:schema_version"`
}
func (IDModel) TableName() string { return "id" }

type ProductModel struct {
	ID        int    `gorm:"primary_key;column:id;"`
	Name      string `gorm:"column:name"`
	Permalink string `gorm:"column:permalink"`
}
func (ProductModel) TableName() string { return "products" }

type CycleModel struct {
	ProductName       string    `gorm:"column:product_name"`
	ProductPermalink  string    `gorm:"column:product_permalink"`
	ID                int       `gorm:"primary_key;column:id;"`
	ProductID         int       `gorm:"column:product_id"`
	ReleaseCycle      string    `gorm:"column:release_cycle"`
	Eol               time.Time `gorm:"column:eol"`
	EolBool           bool      `gorm:"column:eol_bool"`
	LTS               string    `gorm:"column:lts"`
	LatestRelease     string    `gorm:"column:latest_release"`
	LatestReleaseDate time.Time `gorm:"column:latest_release_date"`
	ReleaseDate       time.Time `gorm:"column:release_date"`
}
func (CycleModel) TableName() string { return "cycles" }

type PurlModel struct {
	ID        int    `gorm:"primary_key;column:id;"`
	ProductID int    `gorm:"column:product_id"`
	Purl      string `gorm:"column:purl"`
}
func (PurlModel) TableName() string { return "purls" }

type EOLResponse struct {
	Cycle             string      `json:"cycle"`
	ReleaseDate       string      `json:"releaseDate"`
	Eol               interface{} `json:"eol"` // can be bool or string
	Latest            string      `json:"latest"`
	LatestReleaseDate string      `json:"latestReleaseDate"`
	Lts               interface{} `json:"lts"`
}

func main() {
	dbPath := "xeol.db"
	_ = os.Remove(dbPath)

	fmt.Println("Downloading upstream listing.json...")
	resp, err := http.Get("https://data.xeol.io/xeol/databases/listing.json")
	if err != nil {
		log.Fatalf("failed to fetch listing: %v", err)
	}
	defer resp.Body.Close()

	var listing struct {
		Available map[string][]struct {
			URL   string `json:"url"`
			Built string `json:"built"`
		} `json:"available"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		log.Fatalf("failed to parse listing: %v", err)
	}

	if len(listing.Available["1"]) == 0 {
		log.Fatalf("no available upstream DB found in listing")
	}

	var upstreamURL string
	var maxBuilt time.Time
	for _, item := range listing.Available["1"] {
		t, err := time.Parse(time.RFC3339Nano, item.Built)
		if err == nil && t.After(maxBuilt) {
			maxBuilt = t
			upstreamURL = item.URL
		}
	}

	fmt.Printf("Downloading upstream DB from %s...\n", upstreamURL)

	// Use curl to download to a file, then unpack (it outputs xeol.db and metadata.json).
	// We save to a file first because piping compressed archives to tar can fail on some Linux distributions (like GitHub Actions runners).
	cmd := exec.Command("sh", "-c", fmt.Sprintf("curl -s -L -o temp_upstream_db.tar %s && tar -xf temp_upstream_db.tar && rm temp_upstream_db.tar", upstreamURL))
	if err := cmd.Run(); err != nil {
		log.Fatalf("failed to download and unpack upstream DB: %v", err)
	}

	fmt.Println("Successfully extracted upstream xeol.db. Injecting custom rules...")

	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}

	db.Exec("ALTER TABLE cycles ADD COLUMN product_name text")
	db.Exec("ALTER TABLE cycles ADD COLUMN product_permalink text")

	// Update the build timestamp so xeol clients download it as an update
	db.Exec("UPDATE id SET build_timestamp = ?", time.Now().UTC().Format(time.RFC3339Nano))

	// 2. Seed the Terraform BINARY lifecycle (from endoflife.date)
	seedProduct(db, productSpec{
		Name:      "Terraform",
		Permalink: "terraform",
		Purl:      "pkg:terraform/hashicorp/terraform",
		APIURL:    "https://endoflife.date/api/terraform.json",
	})

	// 3. Seed custom rules from YAML file
	yamlFile, err := os.ReadFile("custom-rules.yaml")
	if err == nil {
		var customProviders []CustomProvider
		if err := yaml.Unmarshal(yamlFile, &customProviders); err != nil {
			log.Fatalf("failed to parse custom-rules.yaml: %v", err)
		}
		for _, cp := range customProviders {
			seedProviderWithStaticCycles(db, productSpec{
				Name:      cp.Name,
				Permalink: cp.Permalink,
				Purl:      cp.Purl,
			}, cp.Cycles)
		}
	} else {
		fmt.Println("No custom-rules.yaml found, skipping custom rule injection.")
	}

	fmt.Println("✅ xeol.db generated successfully with Terraform binary + custom YAML EOL data!")

	// 4. Archive into tar.gz
	now := time.Now().UTC()
	// tar treats colons as remote hosts, so we format the filename without colons
	archiveTime := now.Format("2006-01-02_150405")
	tarName := fmt.Sprintf("xeol-db_v1_%s.tar.gz", archiveTime)

	fmt.Printf("Archiving %s to %s...\n", dbPath, tarName)
	// Create metadata.json
	metadata := map[string]interface{}{
		"built":    now.Format(time.RFC3339),
		"version":  1,
		"checksum": "", // Xeol client doesn't strictly validate this inside metadata.json
	}
	bMeta, _ := json.MarshalIndent(metadata, "", "  ")
	os.WriteFile("metadata.json", bMeta, 0644)

	cmd = exec.Command("tar", "-czf", tarName, dbPath, "metadata.json")
	if err := cmd.Run(); err != nil {
		log.Fatalf("failed to tar database: %v", err)
	}

	// 5. Calculate SHA256 of the tarball
	f, err := os.Open(tarName)
	if err != nil {
		log.Fatalf("failed to open tar: %v", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		log.Fatalf("failed to hash tar: %v", err)
	}
	checksum := fmt.Sprintf("sha256:%x", h.Sum(nil))

	// 6. Generate listing.json
	newListing := map[string]interface{}{
		"available": map[string]interface{}{
			"1": []map[string]interface{}{
				{
					"version":  1,
					"built":    now.Format(time.RFC3339),
					"url":      "https://hellonico.github.io/xeol-open-db/" + tarName,
					"checksum": checksum,
				},
			},
		},
	}

	b, _ := json.MarshalIndent(newListing, "", "  ")
	if err := os.WriteFile("listing.json", b, 0644); err != nil {
		log.Fatalf("failed to write listing.json: %v", err)
	}

	fmt.Println("✅ listing.json and archive generated successfully! Ready for GitHub Pages.")
}

type productSpec struct {
	Name      string
	Permalink string
	Purl      string
	APIURL    string
}

type staticCycle struct {
	Cycle   string `yaml:"cycle"`
	EolDate string `yaml:"eolDate"`
	Latest  string `yaml:"latest"`
}

type CustomProvider struct {
	Name      string        `yaml:"name"`
	Permalink string        `yaml:"permalink"`
	Purl      string        `yaml:"purl"`
	Cycles    []staticCycle `yaml:"cycles"`
}

func seedProduct(db *gorm.DB, spec productSpec) {
	product := ProductModel{Name: spec.Name, Permalink: spec.Permalink}
	db.Create(&product)
	db.Create(&PurlModel{ProductID: product.ID, Purl: spec.Purl})

	resp, err := http.Get(spec.APIURL)
	if err != nil {
		log.Fatalf("failed to fetch %s: %v", spec.APIURL, err)
	}
	defer resp.Body.Close()

	var data []EOLResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		log.Fatalf("failed to decode %s: %v", spec.APIURL, err)
	}

	for _, item := range data {
		cycle := CycleModel{
			ProductID:        product.ID,
			ProductName:      product.Name,
			ProductPermalink: product.Permalink,
			ReleaseCycle:     item.Cycle,
			LatestRelease:    item.Latest,
		}
		if d, err := time.Parse("2006-01-02", item.ReleaseDate); err == nil {
			cycle.ReleaseDate = d
		}
		if d, err := time.Parse("2006-01-02", item.LatestReleaseDate); err == nil {
			cycle.LatestReleaseDate = d
		}
		if eolBool, ok := item.Eol.(bool); ok {
			cycle.EolBool = eolBool
		} else if eolStr, ok := item.Eol.(string); ok {
			if d, err := time.Parse("2006-01-02", eolStr); err == nil {
				cycle.Eol = d
			}
		}
		db.Create(&cycle)
	}
	fmt.Printf("  seeded %s (%d cycles)\n", spec.Name, len(data))
}

func seedProviderWithStaticCycles(db *gorm.DB, spec productSpec, cycles []staticCycle) {
	product := ProductModel{Name: spec.Name, Permalink: spec.Permalink}
	db.Create(&product)
	db.Create(&PurlModel{ProductID: product.ID, Purl: spec.Purl})

	for _, c := range cycles {
		cycle := CycleModel{
			ProductID:        product.ID,
			ProductName:      product.Name,
			ProductPermalink: product.Permalink,
			ReleaseCycle:     c.Cycle,
			LatestRelease:    c.Latest,
		}
		if d, err := time.Parse("2006-01-02", c.EolDate); err == nil {
			cycle.Eol = d
		}
		db.Create(&cycle)
	}
	fmt.Printf("  seeded %s (%d static cycles)\n", spec.Name, len(cycles))
}

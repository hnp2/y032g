package main

import (
    "encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v2"
	"gorm.io/driver/postgres"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Config holds the PostgreSQL connection details.
type Config struct {
	Host     string `yaml:"host"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	DBName   string `yaml:"dbname"`
	Port     int    `yaml:"port"`
	SSLMode  string `yaml:"sslmode"`
}

// Alert represents the alert data stored in PostgreSQL.
type Alert struct {
	ID          uint      `gorm:"primaryKey"`
	Fingerprint string    `gorm:"uniqueIndex"` // Unique identifier for deduplication.
	Status      string
	Labels      datatypes.JSON `gorm:"type:jsonb"` // Stores labels as JSON.
	Annotations datatypes.JSON `gorm:"type:jsonb"` // Stores annotations as JSON.
	StartsAt    time.Time
	EndsAt      time.Time
	CreatedAt   time.Time
}

// AlertWebhook models the JSON payload from Alertmanager.
type AlertWebhook struct {
	Alerts []struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		StartsAt    time.Time         `json:"startsAt"`
		EndsAt      time.Time         `json:"endsAt"`
		Fingerprint string            `json:"fingerprint"`
	} `json:"alerts"`
}

// Prometheus metrics
var (
	irmWebhooksAlertmanagerTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "irm_webhooks_alertmanager_total",
			Help: "Total number of received webhooks",
		},
	)
	irmWebhooksAlertmanagerNewTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "irm_webhooks_alertmanager_new_total",
			Help: "Total number of new unique webhook inserted into the database",
		},
	)
	irmWebhooksAlertmanagerDuplicateTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "irm_webhooks_alertmanager_duplicate_total",
			Help: "Total number of duplicate webhooks (already exists in DB)",
		},
	)
	irmWebhooksAlertmanagerUpdatedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "irm_webhooks_alertmanager_updated_total",
			Help: "Total number of webhooks that were updated",
		},
	)
)

func init() {
	// Register metrics with Prometheus
	prometheus.MustRegister(irmWebhooksAlertmanagerTotal, irmWebhooksAlertmanagerNewTotal, irmWebhooksAlertmanagerDuplicateTotal, irmWebhooksAlertmanagerUpdatedTotal)
}

// loadConfig reads configuration from a YAML file.
func loadConfig(filePath string) (*Config, error) {
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func main() {
	// Accept a config file path as a command-line flag.
	configPath := flag.String("config", "config.yaml", "Path to YAML configuration file")
	flag.Parse()

	// Load the configuration file.
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	// Build the DSN string using the config values.
	dsn := fmt.Sprintf("host=%s user=%s password=%s dbname=%s port=%d sslmode=%s",
		cfg.Host, cfg.User, cfg.Password, cfg.DBName, cfg.Port, cfg.SSLMode)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		panic("failed to connect to database")
	}

	// Automatically migrate the Alert model.
	if err := db.AutoMigrate(&Alert{}); err != nil {
		panic("failed to migrate database")
	}

	// Initialize Gin router.
	router := gin.Default()

	// Health check endpoint
	router.GET("/healthz", func(c *gin.Context) {
		sqlDB, err := db.DB()
		if err != nil || sqlDB.Ping() != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"status": "unhealthy", "error": "database unreachable"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "healthy"})
	})

	// Define the webhook endpoint.
	router.POST("/api/v1/webhooks/alertmanager", func(c *gin.Context) {
		var webhook AlertWebhook
		if err := c.ShouldBindJSON(&webhook); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// Increment total received alerts counter
		irmWebhooksAlertmanagerTotal.Add(float64(len(webhook.Alerts)))

		// Process each alert in the payload.
		for _, alert := range webhook.Alerts {
			// Check if an alert with this fingerprint already exists.
			var existing Alert
			err := db.Where("fingerprint = ?", alert.Fingerprint).First(&existing).Error
			if err != nil {
				if err == gorm.ErrRecordNotFound {
					// Marshal labels and annotations to JSON.
					labelsJSON, err := json.Marshal(alert.Labels)
					if err != nil {
						c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to marshal labels"})
						return
					}
					annotationsJSON, err := json.Marshal(alert.Annotations)
					if err != nil {
						c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to marshal annotations"})
						return
					}

					// Create a new Alert record.
					newAlert := Alert{
						Fingerprint: alert.Fingerprint,
						Status:      alert.Status,
						Labels:      labelsJSON,
						Annotations: annotationsJSON,
						StartsAt:    alert.StartsAt,
						EndsAt:      alert.EndsAt,
						CreatedAt:   time.Now(),
					}
					if err := db.Create(&newAlert).Error; err != nil {
						c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
						return
					}

					// Increment new alerts counter
					irmWebhooksAlertmanagerNewTotal.Inc()
				} else {
					c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
					return
				}
			} else {

				if existing.Status != alert.Status {
					if err := db.Model(&existing).Select("Status", "EndsAt").Updates(Alert{Status: alert.Status,EndsAt: alert.EndsAt}).Error; err != nil {
						c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
						return
					}
					irmWebhooksAlertmanagerUpdatedTotal.Inc()
				} else {
                    irmWebhooksAlertmanagerDuplicateTotal.Inc()
				}
			}
		}

		c.JSON(http.StatusOK, gin.H{"status": "alerts processed"})
	})

	// Prometheus metrics endpoint
	router.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Run the server on port 8080.
	router.Run(":8080")
}

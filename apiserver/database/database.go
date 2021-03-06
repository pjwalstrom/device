package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/GoogleCloudPlatform/cloudsql-proxy/proxy/dialers/postgres"
	_ "github.com/lib/pq"
	"github.com/nais/device/apiserver/cidr"
	log "github.com/sirupsen/logrus"
)

const (
	TunnelCidr = "10.255.240.0/21"
)

type APIServerDB struct {
	Conn *sql.DB
}

type Device struct {
	ID             int
	Serial         string `json:"serial"`
	PSK            string `json:"psk"`
	LastUpdated    *int64 `json:"lastUpdated"`
	KolideLastSeen *int64 `json:"kolideLastSeen"`
	Healthy        *bool  `json:"isHealthy"`
	PublicKey      string `json:"publicKey"`
	IP             string `json:"ip"`
	Username       string `json:"username"`
	Platform       string `json:"platform"`
}

type Gateway struct {
	Endpoint       string   `json:"endpoint"`
	PublicKey      string   `json:"publicKey"`
	IP             string   `json:"ip"`
	Routes         []string `json:"routes"`
	Name           string   `json:"name"`
	AccessGroupIDs []string `json:"-"`
}

type SessionInfo struct {
	Key    string `json:"key"`
	Expiry int64  `json:"expiry"`
	Device *Device
	Groups []string
}

func (si SessionInfo) Expired() bool {
	return time.Unix(si.Expiry, 0).After(time.Now())
}

func New(dsn, driver string) (*APIServerDB, error) {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to database: %s", err)
	}

	return &APIServerDB{Conn: db}, nil
}

func (d *APIServerDB) ReadDevices() ([]Device, error) {
	ctx := context.Background()

	query := `
SELECT public_key, username, ip, psk, serial, platform, healthy, last_updated, kolide_last_seen
FROM device;`

	rows, err := d.Conn.QueryContext(ctx, query)

	if err != nil {
		return nil, fmt.Errorf("querying for devices: %v", err)
	}

	defer rows.Close()

	if rows.Err() != nil {
		return nil, fmt.Errorf("querying for devices: %v", rows.Err())
	}

	devices := []Device{} // don't want nil declaration here as this JSON encodes to 'null' instead of '[]'
	for rows.Next() {
		var device Device

		err := rows.Scan(&device.PublicKey, &device.Username, &device.IP, &device.PSK, &device.Serial, &device.Platform, &device.Healthy, &device.LastUpdated, &device.KolideLastSeen)

		if err != nil {
			return nil, fmt.Errorf("scanning row: %s", err)
		}

		devices = append(devices, device)
	}

	return devices, nil
}

func (d *APIServerDB) UpdateDeviceStatus(devices []Device) error {
	ctx := context.Background()

	tx, err := d.Conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("start transaction: %s", err)
	}

	defer tx.Rollback()

	query := `
		UPDATE device
           SET healthy = $1, kolide_last_seen = $2, last_updated = CAST(EXTRACT(EPOCH FROM NOW()) AS BIGINT)
         WHERE serial = $3 AND platform = $4;
    `

	for _, device := range devices {
		_, err = tx.ExecContext(ctx, query, device.Healthy, device.KolideLastSeen, device.Serial, device.Platform)
		if err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commiting transaction: %w", err)
	}

	log.Infof("Successfully updated device statuses")

	return nil
}

func (d *APIServerDB) AddGateway(ctx context.Context, gateway Gateway) error {
	statement := `
INSERT INTO gateway (name, access_group_ids, endpoint, public_key, ip, routes)
VALUES ($1, $2, $3, $4, $5, $6);`

	_, err := d.Conn.ExecContext(ctx, statement, gateway.Name, strings.Join(gateway.AccessGroupIDs, ","), gateway.Endpoint, gateway.PublicKey, gateway.IP, strings.Join(gateway.Routes, ","))

	if err != nil {
		return fmt.Errorf("inserting new gateway: %w", err)
	}

	log.Infof("Added gateway: %+v", gateway)

	return nil
}

var mux sync.Mutex

func (d *APIServerDB) AddDevice(ctx context.Context, device Device) error {
	mux.Lock()
	defer mux.Unlock()

	tx, err := d.Conn.Begin()
	if err != nil {
		return fmt.Errorf("start transaction: %w", err)
	}

	defer tx.Rollback()

	ips, err := d.readExistingIPs()
	if err != nil {
		return fmt.Errorf("reading existing ips: %w", err)
	}

	ip, err := cidr.FindAvailableIP(TunnelCidr, ips)
	if err != nil {
		return fmt.Errorf("finding available ip: %w", err)
	}

	statement := `
INSERT INTO device (serial, username, public_key, ip, healthy, psk, platform)
VALUES ($1, $2, $3, $4, false, '', $5)
ON CONFLICT(serial, platform) DO UPDATE SET username = $2, public_key = $3;`
	_, err = tx.ExecContext(ctx, statement, device.Serial, device.Username, device.PublicKey, ip, device.Platform)

	if err != nil {
		return fmt.Errorf("inserting new device: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commiting transaction: %w", err)
	}

	log.Infof("Added or updated device: %+v", device)

	return nil
}

func (d *APIServerDB) ReadDevice(publicKey string) (*Device, error) {
	ctx := context.Background()

	query := `
SELECT id, serial, username, psk, platform, last_updated, kolide_last_seen, healthy, public_key, ip
  FROM device
 WHERE public_key = $1;`

	row := d.Conn.QueryRowContext(ctx, query, publicKey)

	var device Device
	err := row.Scan(&device.ID, &device.Serial, &device.Username, &device.PSK, &device.Platform, &device.LastUpdated, &device.KolideLastSeen, &device.Healthy, &device.PublicKey, &device.IP)

	if err != nil {
		return nil, fmt.Errorf("scanning row: %s", err)
	}

	return &device, nil
}

func (d *APIServerDB) ReadDeviceById(ctx context.Context, deviceID int) (*Device, error) {
	query := `
SELECT id, serial, username, psk, platform, last_updated, kolide_last_seen, healthy, public_key, ip
  FROM device
 WHERE id = $1;`

	row := d.Conn.QueryRowContext(ctx, query, deviceID)

	var device Device
	err := row.Scan(&device.ID, &device.Serial, &device.Username, &device.PSK, &device.Platform, &device.LastUpdated, &device.KolideLastSeen, &device.Healthy, &device.PublicKey, &device.IP)

	if err != nil {
		return nil, fmt.Errorf("scanning row: %s", err)
	}

	return &device, nil
}

func (d *APIServerDB) ReadGateways() ([]Gateway, error) {
	ctx := context.Background()

	query := `
SELECT public_key, access_group_ids, endpoint, ip, routes, name
  FROM gateway;`

	rows, err := d.Conn.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("querying for gateways %w", err)
	}

	var gateways []Gateway
	for rows.Next() {
		var gateway Gateway
		var routes string
		var accessGroupIDs string
		err := rows.Scan(&gateway.PublicKey, &accessGroupIDs, &gateway.Endpoint, &gateway.IP, &routes, &gateway.Name)
		if err != nil {
			return nil, fmt.Errorf("scanning gateway: %w", err)
		}

		if len(accessGroupIDs) != 0 {
			gateway.AccessGroupIDs = strings.Split(accessGroupIDs, ",")
		}

		if len(routes) != 0 {
			gateway.Routes = strings.Split(routes, ",")
		}

		gateways = append(gateways, gateway)
	}

	if rows.Err() != nil {
		return nil, fmt.Errorf("iterating over rows: %w", rows.Err())
	}

	return gateways, nil

}

func (d *APIServerDB) ReadGateway(name string) (*Gateway, error) {
	ctx := context.Background()

	query := `
SELECT public_key, access_group_ids, endpoint, ip, routes
  FROM gateway
 WHERE name = $1;`

	row := d.Conn.QueryRowContext(ctx, query, name)

	var gateway Gateway
	var routes string
	var accessGroupIDs string
	err := row.Scan(&gateway.PublicKey, &accessGroupIDs, &gateway.Endpoint, &gateway.IP, &routes)
	if err != nil {
		return nil, fmt.Errorf("scanning gateway: %w", err)
	}

	if len(accessGroupIDs) != 0 {
		gateway.AccessGroupIDs = strings.Split(accessGroupIDs, ",")
	}

	if len(routes) != 0 {
		gateway.Routes = strings.Split(routes, ",")
	}

	return &gateway, nil
}

func (d *APIServerDB) readExistingIPs() ([]string, error) {
	ips := []string{
		"10.255.240.1", // reserve apiserver ip
	}

	if devices, err := d.ReadDevices(); err != nil {
		return nil, fmt.Errorf("reading devices: %w", err)
	} else {
		for _, device := range devices {
			ips = append(ips, device.IP)
		}
	}

	if gateways, err := d.ReadGateways(); err != nil {
		return nil, fmt.Errorf("reading gateways: %w", err)
	} else {
		for _, gateway := range gateways {
			ips = append(ips, gateway.IP)
		}
	}

	return ips, nil
}

func (d *APIServerDB) ReadDeviceBySerialPlatformUsername(ctx context.Context, serial string, platform string, username string) (*Device, error) {
	query := `
SELECT id, username, serial, psk, platform, healthy, last_updated, kolide_last_seen, public_key, ip
  FROM device
 WHERE serial = $1
   AND platform = $2
   AND username = $3;
	`

	var device Device
	row := d.Conn.QueryRowContext(ctx, query, serial, platform, username)

	err := row.Scan(&device.ID, &device.Username, &device.Serial, &device.PSK, &device.Platform, &device.Healthy, &device.LastUpdated, &device.KolideLastSeen, &device.PublicKey, &device.IP)

	if err != nil {
		return nil, fmt.Errorf("scanning row: %s", err)
	}

	return &device, nil
}

func (d *APIServerDB) AddSessionInfo(ctx context.Context, si *SessionInfo) error {
	query := `
INSERT INTO session (key, expiry, device_id, groups)
             VALUES ($1, $2, $3, $4);
`

	_, err := d.Conn.ExecContext(ctx, query, si.Key, si.Expiry, si.Device.ID, strings.Join(si.Groups, ","))
	if err != nil {
		return fmt.Errorf("scanning row: %s", err)
	}

	log.Infof("persisted session: %v", si)

	return nil
}

func (d *APIServerDB) ReadSessionInfo(ctx context.Context, key string) (*SessionInfo, error) {
	query := `
SELECT key, expiry, device_id, groups
FROM session
WHERE key = $1;
`

	row := d.Conn.QueryRowContext(ctx, query, key)

	var si SessionInfo
	var groups string
	var deviceID int
	err := row.Scan(&si.Key, &si.Expiry, &deviceID, &groups)
	if err != nil {
		return nil, fmt.Errorf("scanning row: %w", err)
	}

	si.Groups = strings.Split(groups, ",")
	si.Device, err = d.ReadDeviceById(ctx, deviceID)

	if err != nil {
		return nil, fmt.Errorf("reading device: %w", err)
	}

	log.Infof("retrieved session info from db: %v", si)

	return &si, nil
}

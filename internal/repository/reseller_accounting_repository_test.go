package repository

import (
	"fmt"
	"testing"
	"time"

	"github.com/dujiao-next/internal/models"
	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func openResellerAccountingRepoTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:reseller_accounting_repo_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	if err := db.AutoMigrate(
		&models.User{},
		&models.Order{},
		&models.Payment{},
		&models.ResellerProfile{},
		&models.ResellerOrderSnapshot{},
		&models.ResellerLedgerEntry{},
		&models.ResellerWithdrawRequest{},
		&models.ResellerBalanceAccount{},
	); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}
	return db
}

func seedResellerAccountingProfile(t *testing.T, db *gorm.DB) models.ResellerProfile {
	t.Helper()
	user := models.User{Email: fmt.Sprintf("reseller-%d@example.test", time.Now().UnixNano()), PasswordHash: "x"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("create user failed: %v", err)
	}
	profile := models.ResellerProfile{
		UserID:           user.ID,
		Status:           models.ResellerProfileStatusActive,
		SettlementStatus: models.ResellerSettlementStatusNormal,
	}
	if err := db.Create(&profile).Error; err != nil {
		t.Fatalf("create reseller profile failed: %v", err)
	}
	return profile
}

func TestResellerAccountingRepositoryLedgerIdempotency(t *testing.T) {
	db := openResellerAccountingRepoTestDB(t)
	profile := seedResellerAccountingProfile(t, db)
	repo := NewResellerRepository(db)
	orderID := uint(100)
	entry := &models.ResellerLedgerEntry{
		ResellerID:     profile.ID,
		OrderID:        &orderID,
		Type:           models.ResellerLedgerTypeOrderProfit,
		Amount:         models.NewMoneyFromDecimal(decimal.RequireFromString("12.34")),
		Currency:       "USD",
		IdempotencyKey: "order_profit:100",
		Status:         models.ResellerLedgerStatusPendingConfirm,
	}
	created, err := repo.CreateLedgerEntryIfNotExists(entry)
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}
	if !created {
		t.Fatal("first create should report created=true")
	}
	created, err = repo.CreateLedgerEntryIfNotExists(entry)
	if err != nil {
		t.Fatalf("second create failed: %v", err)
	}
	if created {
		t.Fatal("second create should report created=false")
	}
	var count int64
	if err := db.Model(&models.ResellerLedgerEntry{}).Where("idempotency_key = ?", "order_profit:100").Count(&count).Error; err != nil {
		t.Fatalf("count ledger failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected one ledger row, got %d", count)
	}
}

func TestResellerAccountingRepositoryMarkDueLedgersAvailable(t *testing.T) {
	db := openResellerAccountingRepoTestDB(t)
	profile := seedResellerAccountingProfile(t, db)
	repo := NewResellerRepository(db)
	now := time.Now()
	past := now.Add(-time.Minute)
	future := now.Add(time.Minute)
	rows := []models.ResellerLedgerEntry{
		{ResellerID: profile.ID, Type: models.ResellerLedgerTypeOrderProfit, Amount: models.NewMoneyFromDecimal(decimal.NewFromInt(10)), Currency: "USD", IdempotencyKey: "order_profit:1", Status: models.ResellerLedgerStatusPendingConfirm, AvailableAt: &past},
		{ResellerID: profile.ID, Type: models.ResellerLedgerTypeOrderProfit, Amount: models.NewMoneyFromDecimal(decimal.NewFromInt(20)), Currency: "USD", IdempotencyKey: "order_profit:2", Status: models.ResellerLedgerStatusPendingConfirm, AvailableAt: &future},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed ledger rows failed: %v", err)
	}
	affected, err := repo.MarkDueLedgerEntriesAvailable(now)
	if err != nil {
		t.Fatalf("mark due failed: %v", err)
	}
	if affected != 1 {
		t.Fatalf("expected affected=1, got %d", affected)
	}
	var due models.ResellerLedgerEntry
	if err := db.First(&due, rows[0].ID).Error; err != nil {
		t.Fatalf("load due row failed: %v", err)
	}
	if due.Status != models.ResellerLedgerStatusAvailable {
		t.Fatalf("expected due row available, got %s", due.Status)
	}
}

func TestResellerAccountingRepositoryWithdrawLocksSameCurrencyOnly(t *testing.T) {
	db := openResellerAccountingRepoTestDB(t)
	profile := seedResellerAccountingProfile(t, db)
	repo := NewResellerRepository(db)
	now := time.Now()
	rows := []models.ResellerLedgerEntry{
		{ResellerID: profile.ID, Type: models.ResellerLedgerTypeOrderProfit, Amount: models.NewMoneyFromDecimal(decimal.NewFromInt(10)), Currency: "USD", IdempotencyKey: "order_profit:usd1", Status: models.ResellerLedgerStatusAvailable, AvailableAt: &now},
		{ResellerID: profile.ID, Type: models.ResellerLedgerTypeOrderProfit, Amount: models.NewMoneyFromDecimal(decimal.NewFromInt(20)), Currency: "CNY", IdempotencyKey: "order_profit:cny1", Status: models.ResellerLedgerStatusAvailable, AvailableAt: &now},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed ledger rows failed: %v", err)
	}
	locked, err := repo.ListAvailableLedgerEntriesForUpdate(profile.ID, "USD")
	if err != nil {
		t.Fatalf("list available ledgers failed: %v", err)
	}
	if len(locked) != 1 || locked[0].Currency != "USD" {
		t.Fatalf("expected only USD ledger, got %+v", locked)
	}
}

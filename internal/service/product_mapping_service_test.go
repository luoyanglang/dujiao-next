package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/models"
	"github.com/dujiao-next/internal/repository"
	"github.com/dujiao-next/internal/upstream"
	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type failingSKUMappingRepo struct {
	err error
}

func (r *failingSKUMappingRepo) GetByID(id uint) (*models.SKUMapping, error) {
	return nil, nil
}

func (r *failingSKUMappingRepo) GetByLocalSKUID(skuID uint) (*models.SKUMapping, error) {
	return nil, nil
}

func (r *failingSKUMappingRepo) GetByMappingAndUpstreamSKUID(productMappingID, upstreamSKUID uint) (*models.SKUMapping, error) {
	return nil, nil
}

func (r *failingSKUMappingRepo) ListByProductMapping(productMappingID uint) ([]models.SKUMapping, error) {
	return nil, nil
}

func (r *failingSKUMappingRepo) ListByProductMappingIDs(productMappingIDs []uint) ([]models.SKUMapping, error) {
	return nil, nil
}

func (r *failingSKUMappingRepo) WithTx(tx *gorm.DB) repository.SKUMappingRepository {
	return r
}

func (r *failingSKUMappingRepo) Create(mapping *models.SKUMapping) error {
	return r.err
}

func (r *failingSKUMappingRepo) Update(mapping *models.SKUMapping) error {
	return nil
}

func (r *failingSKUMappingRepo) Delete(id uint) error {
	return nil
}

func (r *failingSKUMappingRepo) DeleteByProductMapping(productMappingID uint) error {
	return nil
}

func (r *failingSKUMappingRepo) BatchUpsert(mappings []models.SKUMapping) error {
	return r.err
}

func TestImportUpstreamProductRollbackWhenSKUMappingCreateFails(t *testing.T) {
	dsn := "file:product_mapping_import_rollback?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	if err := db.AutoMigrate(
		&models.Category{},
		&models.Product{},
		&models.ProductSKU{},
		&models.SiteConnection{},
		&models.ProductMapping{},
	); err != nil {
		t.Fatalf("auto migrate failed: %v", err)
	}

	categoryRepo := repository.NewCategoryRepository(db)
	if err := categoryRepo.Create(&models.Category{
		ParentID: 0,
		Slug:     "test-category",
		NameJSON: models.JSON{"zh-CN": "Test Category"},
	}); err != nil {
		t.Fatalf("create category failed: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/upstream/products/101" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"product": upstream.UpstreamProduct{
				ID:              101,
				Title:           models.JSON{"zh-CN": "映射测试商品"},
				Description:     models.JSON{"zh-CN": "描述"},
				Content:         models.JSON{"zh-CN": "内容"},
				Images:          []string{},
				Tags:            []string{"tag-a"},
				PriceAmount:     "10.00",
				Currency:        "CNY",
				FulfillmentType: constants.FulfillmentTypeAuto,
				IsActive:        true,
				SKUs: []upstream.UpstreamSKU{
					{
						ID:          201,
						SKUCode:     "SKU-A",
						SpecValues:  models.JSON{"name": "A"},
						PriceAmount: "10.00",
						IsActive:    true,
					},
				},
			},
		})
	}))
	defer server.Close()

	connService := NewSiteConnectionService(repository.NewSiteConnectionRepository(db), "test-secret-key", t.TempDir())
	conn, err := connService.Create(CreateConnectionInput{
		Name:      "upstream-a",
		BaseURL:   server.URL,
		ApiKey:    "test-key",
		ApiSecret: "test-secret",
		Protocol:  constants.ConnectionProtocolDujiaoNext,
	})
	if err != nil {
		t.Fatalf("create connection failed: %v", err)
	}

	svc := NewProductMappingService(
		repository.NewProductMappingRepository(db),
		&failingSKUMappingRepo{err: errors.New("inject sku mapping failure")},
		repository.NewProductRepository(db),
		repository.NewProductSKURepository(db),
		categoryRepo,
		connService,
	)

	if _, err := svc.ImportUpstreamProduct(conn.ID, 101, 1, "rollback-slug"); err == nil {
		t.Fatalf("expected import upstream product to fail")
	}

	var productCount int64
	if err := db.Model(&models.Product{}).Count(&productCount).Error; err != nil {
		t.Fatalf("count products failed: %v", err)
	}
	if productCount != 0 {
		t.Fatalf("expected product rollback, got %d products", productCount)
	}

	var skuCount int64
	if err := db.Model(&models.ProductSKU{}).Count(&skuCount).Error; err != nil {
		t.Fatalf("count product skus failed: %v", err)
	}
	if skuCount != 0 {
		t.Fatalf("expected sku rollback, got %d skus", skuCount)
	}

	var mappingCount int64
	if err := db.Model(&models.ProductMapping{}).Count(&mappingCount).Error; err != nil {
		t.Fatalf("count product mappings failed: %v", err)
	}
	if mappingCount != 0 {
		t.Fatalf("expected mapping rollback, got %d mappings", mappingCount)
	}
}

// setupMappingWithUpstreamHandler 准备一份本地映射 + 启动可定制响应的上游 httptest server
func setupMappingWithUpstreamHandler(t *testing.T, dsn string, handler http.HandlerFunc) (*ProductMappingService, *gorm.DB, *models.ProductMapping, func()) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	if err := db.AutoMigrate(
		&models.Category{},
		&models.Product{},
		&models.ProductSKU{},
		&models.SiteConnection{},
		&models.ProductMapping{},
		&models.SKUMapping{},
	); err != nil {
		t.Fatalf("auto migrate failed: %v", err)
	}

	server := httptest.NewServer(handler)

	categoryRepo := repository.NewCategoryRepository(db)
	if err := categoryRepo.Create(&models.Category{Slug: "c", NameJSON: models.JSON{"zh-CN": "C"}}); err != nil {
		t.Fatalf("create category failed: %v", err)
	}

	productRepo := repository.NewProductRepository(db)
	product := models.Product{
		CategoryID:      1,
		Slug:            "p",
		TitleJSON:       models.JSON{"zh-CN": "P"},
		PriceAmount:     models.NewMoneyFromDecimal(decimal.NewFromInt(10)),
		FulfillmentType: constants.FulfillmentTypeUpstream,
		IsActive:        true,
		IsMapped:        true,
	}
	if err := productRepo.Create(&product); err != nil {
		t.Fatalf("create product failed: %v", err)
	}
	skuRepo := repository.NewProductSKURepository(db)
	sku := models.ProductSKU{ProductID: product.ID, SKUCode: "SKU-A", PriceAmount: models.NewMoneyFromDecimal(decimal.NewFromInt(10)), IsActive: true}
	if err := skuRepo.Create(&sku); err != nil {
		t.Fatalf("create sku failed: %v", err)
	}

	connService := NewSiteConnectionService(repository.NewSiteConnectionRepository(db), "test-secret-key", t.TempDir())
	conn, err := connService.Create(CreateConnectionInput{
		Name:      "upstream",
		BaseURL:   server.URL,
		ApiKey:    "k",
		ApiSecret: "s",
		Protocol:  constants.ConnectionProtocolDujiaoNext,
	})
	if err != nil {
		t.Fatalf("create connection failed: %v", err)
	}

	mappingRepo := repository.NewProductMappingRepository(db)
	skuMappingRepo := repository.NewSKUMappingRepository(db)
	mapping := &models.ProductMapping{
		ConnectionID:      conn.ID,
		LocalProductID:    product.ID,
		UpstreamProductID: 101,
		IsActive:          true,
		UpstreamStatus:    models.UpstreamStatusActive,
	}
	if err := mappingRepo.Create(mapping); err != nil {
		t.Fatalf("create mapping failed: %v", err)
	}
	if err := skuMappingRepo.Create(&models.SKUMapping{
		ProductMappingID: mapping.ID,
		LocalSKUID:       sku.ID,
		UpstreamSKUID:    201,
		UpstreamIsActive: true,
		UpstreamStock:    100,
	}); err != nil {
		t.Fatalf("create sku mapping failed: %v", err)
	}

	svc := NewProductMappingService(mappingRepo, skuMappingRepo, productRepo, skuRepo, categoryRepo, connService)
	return svc, db, mapping, server.Close
}

func TestSyncProductMarksDeletedWhenUpstreamSoftDeleted(t *testing.T) {
	svc, db, mapping, cleanup := setupMappingWithUpstreamHandler(t,
		"file:sync_deleted?mode=memory&cache=shared",
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":            false,
				"error_code":    "product_deleted",
				"error_message": "product has been deleted",
			})
		},
	)
	defer cleanup()

	if err := svc.SyncProduct(mapping.ID); err != nil {
		t.Fatalf("SyncProduct returned error: %v", err)
	}

	var got models.ProductMapping
	if err := db.First(&got, mapping.ID).Error; err != nil {
		t.Fatalf("reload mapping failed: %v", err)
	}
	if got.UpstreamStatus != models.UpstreamStatusDeleted {
		t.Fatalf("expected upstream_status=deleted, got %q", got.UpstreamStatus)
	}
	if got.IsActive {
		t.Fatalf("expected mapping to be deactivated for deleted upstream")
	}

	var product models.Product
	if err := db.First(&product, mapping.LocalProductID).Error; err != nil {
		t.Fatalf("reload product failed: %v", err)
	}
	if product.IsActive {
		t.Fatalf("expected local product to be deactivated")
	}

	var skuMapping models.SKUMapping
	if err := db.Where("product_mapping_id = ?", mapping.ID).First(&skuMapping).Error; err != nil {
		t.Fatalf("reload sku mapping failed: %v", err)
	}
	if skuMapping.UpstreamIsActive || skuMapping.UpstreamStock != 0 {
		t.Fatalf("expected sku mapping to be marked unavailable, got is_active=%v stock=%d", skuMapping.UpstreamIsActive, skuMapping.UpstreamStock)
	}
}

func TestSyncProductMarksInactiveWhenUpstreamReturnsInactive(t *testing.T) {
	svc, db, mapping, cleanup := setupMappingWithUpstreamHandler(t,
		"file:sync_inactive?mode=memory&cache=shared",
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"product": upstream.UpstreamProduct{
					ID:       101,
					IsActive: false, // 上游下架
					SKUs: []upstream.UpstreamSKU{
						{ID: 201, SKUCode: "SKU-A", PriceAmount: "10.00", IsActive: false},
					},
				},
			})
		},
	)
	defer cleanup()

	if err := svc.SyncProduct(mapping.ID); err != nil {
		t.Fatalf("SyncProduct returned error: %v", err)
	}

	var got models.ProductMapping
	if err := db.First(&got, mapping.ID).Error; err != nil {
		t.Fatalf("reload mapping failed: %v", err)
	}
	if got.UpstreamStatus != models.UpstreamStatusInactive {
		t.Fatalf("expected upstream_status=inactive, got %q", got.UpstreamStatus)
	}
	if !got.IsActive {
		t.Fatalf("expected mapping to remain active for inactive upstream (only deleted should auto-disable)")
	}

	var product models.Product
	if err := db.First(&product, mapping.LocalProductID).Error; err != nil {
		t.Fatalf("reload product failed: %v", err)
	}
	if product.IsActive {
		t.Fatalf("expected local product to be deactivated")
	}
}

// listProductsHandler 构造一个 /api/v1/upstream/products 列表响应 handler
func listProductsHandler(items []upstream.UpstreamProduct, includesInactive bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":                true,
			"items":             items,
			"total":             len(items),
			"page":              1,
			"page_size":         50,
			"includes_inactive": includesInactive,
		})
	}
}

func TestSyncConnectionStockMarksDeletedWhenFullSyncMissing(t *testing.T) {
	// 上游 ListProducts 返回空列表 + includes_inactive=true →
	// 全量模式下 mapping 在列表中 missing 必定意味着上游已软删
	svc, db, mapping, cleanup := setupMappingWithUpstreamHandler(t,
		"file:sync_full_missing_deleted?mode=memory&cache=shared",
		listProductsHandler([]upstream.UpstreamProduct{}, true),
	)
	defer cleanup()

	if err := svc.syncConnectionStock(mapping.ConnectionID, []models.ProductMapping{*mapping}); err != nil {
		t.Fatalf("syncConnectionStock returned error: %v", err)
	}

	var got models.ProductMapping
	if err := db.First(&got, mapping.ID).Error; err != nil {
		t.Fatalf("reload mapping failed: %v", err)
	}
	if got.UpstreamStatus != models.UpstreamStatusDeleted {
		t.Fatalf("expected upstream_status=deleted, got %q", got.UpstreamStatus)
	}
	if got.IsActive {
		t.Fatalf("expected mapping to be deactivated when upstream marks deleted")
	}

	var product models.Product
	if err := db.First(&product, mapping.LocalProductID).Error; err != nil {
		t.Fatalf("reload product failed: %v", err)
	}
	if product.IsActive {
		t.Fatalf("expected local product to be deactivated")
	}
}

func TestSyncConnectionStockKeepsLegacyUpstreamMissing(t *testing.T) {
	// 上游空列表 + includes_inactive=false（旧上游不支持新参数）→
	// 不能据此推断"missing=已删除"，本地状态应保持不变
	svc, db, mapping, cleanup := setupMappingWithUpstreamHandler(t,
		"file:sync_full_missing_legacy?mode=memory&cache=shared",
		listProductsHandler([]upstream.UpstreamProduct{}, false),
	)
	defer cleanup()

	if err := svc.syncConnectionStock(mapping.ConnectionID, []models.ProductMapping{*mapping}); err != nil {
		t.Fatalf("syncConnectionStock returned error: %v", err)
	}

	var got models.ProductMapping
	if err := db.First(&got, mapping.ID).Error; err != nil {
		t.Fatalf("reload mapping failed: %v", err)
	}
	if got.UpstreamStatus != models.UpstreamStatusActive {
		t.Fatalf("legacy upstream missing must not change status, got %q", got.UpstreamStatus)
	}
	if !got.IsActive {
		t.Fatalf("legacy upstream missing must not deactivate mapping")
	}

	var product models.Product
	if err := db.First(&product, mapping.LocalProductID).Error; err != nil {
		t.Fatalf("reload product failed: %v", err)
	}
	if !product.IsActive {
		t.Fatalf("legacy upstream missing must not deactivate local product")
	}
}

func TestSyncProductRestoresStatusWhenUpstreamRecovers(t *testing.T) {
	// 之前已被标 inactive，上游 GetProduct 返回 IsActive=true → UpstreamStatus 应恢复为 active
	svc, db, mapping, cleanup := setupMappingWithUpstreamHandler(t,
		"file:sync_recover_active?mode=memory&cache=shared",
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"product": upstream.UpstreamProduct{
					ID:              101,
					Title:           models.JSON{"zh-CN": "P"},
					PriceAmount:     "10.00",
					Currency:        "CNY",
					FulfillmentType: constants.FulfillmentTypeAuto,
					IsActive:        true,
					SKUs: []upstream.UpstreamSKU{
						{ID: 201, SKUCode: "SKU-A", PriceAmount: "10.00", IsActive: true, StockQuantity: 50},
					},
				},
			})
		},
	)
	defer cleanup()

	// 先把 mapping 状态改为 inactive 模拟"之前已下架"
	if err := db.Model(&models.ProductMapping{}).Where("id = ?", mapping.ID).
		Update("upstream_status", models.UpstreamStatusInactive).Error; err != nil {
		t.Fatalf("preset inactive failed: %v", err)
	}

	if err := svc.SyncProduct(mapping.ID); err != nil {
		t.Fatalf("SyncProduct returned error: %v", err)
	}

	var got models.ProductMapping
	if err := db.First(&got, mapping.ID).Error; err != nil {
		t.Fatalf("reload mapping failed: %v", err)
	}
	if got.UpstreamStatus != models.UpstreamStatusActive {
		t.Fatalf("expected upstream_status to recover to active, got %q", got.UpstreamStatus)
	}
}

func TestImportUpstreamProductRejectsInactive(t *testing.T) {
	// 上游 GetProduct 返回 200 + is_active=false → 拒绝导入
	dsn := "file:import_reject_inactive?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	if err := db.AutoMigrate(
		&models.Category{},
		&models.Product{},
		&models.ProductSKU{},
		&models.SiteConnection{},
		&models.ProductMapping{},
		&models.SKUMapping{},
	); err != nil {
		t.Fatalf("auto migrate failed: %v", err)
	}

	categoryRepo := repository.NewCategoryRepository(db)
	if err := categoryRepo.Create(&models.Category{Slug: "c", NameJSON: models.JSON{"zh-CN": "C"}}); err != nil {
		t.Fatalf("create category failed: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"product": upstream.UpstreamProduct{
				ID:          202,
				Title:       models.JSON{"zh-CN": "已下架商品"},
				PriceAmount: "10.00",
				IsActive:    false,
			},
		})
	}))
	defer server.Close()

	connService := NewSiteConnectionService(repository.NewSiteConnectionRepository(db), "test-secret-key", t.TempDir())
	conn, err := connService.Create(CreateConnectionInput{
		Name: "u", BaseURL: server.URL, ApiKey: "k", ApiSecret: "s",
		Protocol: constants.ConnectionProtocolDujiaoNext,
	})
	if err != nil {
		t.Fatalf("create connection failed: %v", err)
	}

	svc := NewProductMappingService(
		repository.NewProductMappingRepository(db),
		repository.NewSKUMappingRepository(db),
		repository.NewProductRepository(db),
		repository.NewProductSKURepository(db),
		categoryRepo,
		connService,
	)

	_, importErr := svc.ImportUpstreamProduct(conn.ID, 202, 1, "")
	if !errors.Is(importErr, ErrUpstreamProductNotFound) {
		t.Fatalf("expected ErrUpstreamProductNotFound for inactive upstream product, got %v", importErr)
	}

	var productCount int64
	if err := db.Model(&models.Product{}).Count(&productCount).Error; err != nil {
		t.Fatalf("count products failed: %v", err)
	}
	if productCount != 0 {
		t.Fatalf("expected no local product created when import rejected, got %d", productCount)
	}
}

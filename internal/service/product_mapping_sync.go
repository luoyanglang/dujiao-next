package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/dujiao-next/internal/cache"
	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/logger"
	"github.com/dujiao-next/internal/models"
	"github.com/dujiao-next/internal/upstream"

	"github.com/shopspring/decimal"
)

// SyncProduct 同步单个映射商品的上游数据（全量同步）
func (s *ProductMappingService) SyncProduct(mappingID uint) error {
	mapping, err := s.mappingRepo.GetByID(mappingID)
	if err != nil {
		return err
	}
	if mapping == nil {
		return ErrMappingNotFound
	}

	conn, err := s.connService.GetByID(mapping.ConnectionID)
	if err != nil {
		return err
	}
	if conn == nil {
		return ErrConnectionNotFound
	}

	adapter, err := s.connService.GetAdapter(conn)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	upProduct, err := adapter.GetProduct(ctx, mapping.UpstreamProductID)
	if err != nil {
		// 上游软删除 → 标记本地为 deleted，自动停用映射
		if errors.Is(err, upstream.ErrUpstreamProductDeleted) {
			now := time.Now()
			return s.markUpstreamUnavailable(mapping, models.UpstreamStatusDeleted, now)
		}
		// 旧版上游下架兜底（新版上游下架返回 200 + is_active=false，走下方分支）
		if errors.Is(err, upstream.ErrUpstreamProductUnavailable) {
			now := time.Now()
			return s.markUpstreamUnavailable(mapping, models.UpstreamStatusInactive, now)
		}
		return fmt.Errorf("fetch upstream product: %w", err)
	}

	now := time.Now()

	// 上游 200 但 is_active=false → 视为下架
	if !upProduct.IsActive {
		return s.markUpstreamUnavailable(mapping, models.UpstreamStatusInactive, now)
	}

	// ── 1. 同步本地商品字段（表单配置、上下架状态） ──
	localProduct, err := s.productRepo.GetByID(strconv.FormatUint(uint64(mapping.LocalProductID), 10))
	if err != nil {
		return fmt.Errorf("get local product: %w", err)
	}
	if localProduct != nil {
		// 同步人工交付表单配置
		if upProduct.ManualFormSchema != nil {
			localProduct.ManualFormSchemaJSON = upProduct.ManualFormSchema
			_ = s.productRepo.Update(localProduct)
		}
	}

	// ── 2. 同步 SKU：新增 / 更新 / 停用 ──
	skuMappings, err := s.skuMappingRepo.ListByProductMapping(mappingID)
	if err != nil {
		return err
	}

	// 构建上游 SKU 查找表
	upstreamSKUMap := make(map[uint]upstream.UpstreamSKU, len(upProduct.SKUs))
	for _, us := range upProduct.SKUs {
		upstreamSKUMap[us.ID] = us
	}

	// 构建已有映射查找表（按上游 SKU ID）
	existingByUpstreamID := make(map[uint]*models.SKUMapping, len(skuMappings))
	for i := range skuMappings {
		existingByUpstreamID[skuMappings[i].UpstreamSKUID] = &skuMappings[i]
	}

	// 2a. 更新已有映射 + 同步本地 SKU
	for i := range skuMappings {
		upSKU, ok := upstreamSKUMap[skuMappings[i].UpstreamSKUID]
		if !ok {
			// 上游 SKU 已删除 → 停用本地 SKU 和映射
			skuMappings[i].UpstreamIsActive = false
			skuMappings[i].UpstreamStock = 0
			skuMappings[i].StockSyncedAt = &now
			_ = s.skuMappingRepo.Update(&skuMappings[i])

			// 停用本地 SKU
			localSKU, _ := s.productSKURepo.GetByID(skuMappings[i].LocalSKUID)
			if localSKU != nil && localSKU.IsActive {
				localSKU.IsActive = false
				_ = s.productSKURepo.Update(localSKU)
			}
			continue
		}

		upPrice, _ := decimal.NewFromString(upSKU.PriceAmount)

		// 更新 SKU 映射记录
		skuMappings[i].UpstreamPrice = models.NewMoneyFromDecimal(upPrice.Round(2))
		skuMappings[i].UpstreamIsActive = upSKU.IsActive
		skuMappings[i].StockSyncedAt = &now
		skuMappings[i].UpstreamStock = upSKU.StockQuantity
		_ = s.skuMappingRepo.Update(&skuMappings[i])

		// 同步本地 SKU 字段
		localSKU, _ := s.productSKURepo.GetByID(skuMappings[i].LocalSKUID)
		if localSKU != nil {
			localSKU.SpecValuesJSON = upSKU.SpecValues
			localSKU.IsActive = upSKU.IsActive
			// 如果启用了自动同步价格，按加价比例更新本地售价和成本价
			if conn.AutoSyncPrice {
				newLocalPrice := CalculateLocalPrice(upPrice, conn.ExchangeRate, conn.PriceMarkupPercent, conn.PriceRoundingMode)
				localSKU.PriceAmount = models.NewMoneyFromDecimal(newLocalPrice.Round(2))
				localSKU.CostPriceAmount = models.NewMoneyFromDecimal(convertCurrency(upPrice, conn.ExchangeRate).Round(2))
			}
			_ = s.productSKURepo.Update(localSKU)
		}
	}

	// 2b. 上游新增的 SKU → 创建本地 SKU + 映射
	for _, upSKU := range upProduct.SKUs {
		if _, exists := existingByUpstreamID[upSKU.ID]; exists {
			continue
		}

		skuPrice, _ := decimal.NewFromString(upSKU.PriceAmount)
		localPrice := CalculateLocalPrice(skuPrice, conn.ExchangeRate, conn.PriceMarkupPercent, conn.PriceRoundingMode)
		newLocalSKU := models.ProductSKU{
			ProductID:       mapping.LocalProductID,
			SKUCode:         upSKU.SKUCode,
			SpecValuesJSON:  upSKU.SpecValues,
			PriceAmount:     models.NewMoneyFromDecimal(localPrice.Round(2)),
			CostPriceAmount: models.NewMoneyFromDecimal(convertCurrency(skuPrice, conn.ExchangeRate).Round(2)), // 成本价 = 上游价格 × 汇率（本地币种）
			IsActive:        upSKU.IsActive,
			SortOrder:       0,
		}
		if err := s.productSKURepo.Create(&newLocalSKU); err != nil {
			continue
		}

		newMapping := &models.SKUMapping{
			ProductMappingID: mappingID,
			LocalSKUID:       newLocalSKU.ID,
			UpstreamSKUID:    upSKU.ID,
			UpstreamPrice:    models.NewMoneyFromDecimal(skuPrice.Round(2)),
			UpstreamIsActive: upSKU.IsActive,
			UpstreamStock:    upSKU.StockQuantity,
			StockSyncedAt:    &now,
		}
		_ = s.skuMappingRepo.Create(newMapping)
	}

	// ── 2c. 如果启用了自动同步价格，更新 Product.PriceAmount 为最低 SKU 价格 ──
	if conn.AutoSyncPrice && localProduct != nil {
		s.recalcProductPrice(localProduct)
	}

	// ── 3. 更新同步时间 + 上游交付类型 + 状态恢复 ──
	upFulfillment := upProduct.FulfillmentType
	if upFulfillment != constants.FulfillmentTypeAuto {
		upFulfillment = constants.FulfillmentTypeManual
	}
	mapping.UpstreamFulfillmentType = upFulfillment
	mapping.UpstreamStatus = models.UpstreamStatusActive
	mapping.LastSyncedAt = &now
	return s.mappingRepo.Update(mapping)
}

// markUpstreamUnavailable 上游下架/删除时的统一处理
// status: models.UpstreamStatusInactive(下架) / models.UpstreamStatusDeleted(已删除)
//   - 本地 Product 下架（IsActive=false），不删除
//   - 所有 SKUMapping 标记为 UpstreamIsActive=false, UpstreamStock=0
//   - 所有本地 SKU 下架
//   - mapping.UpstreamStatus 写入对应状态
//   - status==deleted 时同时停用映射（IsActive=false），避免后续白白调上游
func (s *ProductMappingService) markUpstreamUnavailable(mapping *models.ProductMapping, status string, now time.Time) error {
	// 本地商品下架
	localProduct, err := s.productRepo.GetByID(strconv.FormatUint(uint64(mapping.LocalProductID), 10))
	if err == nil && localProduct != nil && localProduct.IsActive {
		localProduct.IsActive = false
		_ = s.productRepo.Update(localProduct)
	}

	// SKU 映射 + 本地 SKU 下架
	skuMappings, _ := s.skuMappingRepo.ListByProductMapping(mapping.ID)
	for i := range skuMappings {
		skuMappings[i].UpstreamIsActive = false
		skuMappings[i].UpstreamStock = 0
		skuMappings[i].StockSyncedAt = &now
		_ = s.skuMappingRepo.Update(&skuMappings[i])

		localSKU, _ := s.productSKURepo.GetByID(skuMappings[i].LocalSKUID)
		if localSKU != nil && localSKU.IsActive {
			localSKU.IsActive = false
			_ = s.productSKURepo.Update(localSKU)
		}
	}

	mapping.UpstreamStatus = status
	mapping.LastSyncedAt = &now
	if status == models.UpstreamStatusDeleted {
		mapping.IsActive = false
	}
	if err := s.mappingRepo.Update(mapping); err != nil {
		return err
	}

	logger.Infow("upstream_product_unavailable",
		"mapping_id", mapping.ID,
		"connection_id", mapping.ConnectionID,
		"upstream_product_id", mapping.UpstreamProductID,
		"local_product_id", mapping.LocalProductID,
		"status", status,
	)
	return nil
}

// SyncAllStock 同步所有活跃映射的库存（供定时任务调用）
// 使用 Redis 锁防止任务重叠执行，并发调用上游 API 提升吞吐量
func (s *ProductMappingService) SyncAllStock() error {
	ctx := context.Background()
	const lockKey = "upstream:sync_stock_running"

	locked, err := cache.SetNX(ctx, lockKey, "1", 30*time.Minute)
	if err != nil {
		logger.Warnw("sync_stock_lock_error", "error", err)
		// Redis 不可用时降级为直接执行
	} else if !locked {
		logger.Debugw("sync_stock_skip_already_running")
		return nil
	}
	defer cache.Del(ctx, lockKey)

	mappings, err := s.mappingRepo.ListAllActive()
	if err != nil {
		return err
	}
	if len(mappings) == 0 {
		return nil
	}

	// ── 按连接分组 ──
	byConn := make(map[uint][]models.ProductMapping)
	for _, m := range mappings {
		byConn[m.ConnectionID] = append(byConn[m.ConnectionID], m)
	}

	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup

	// 每个连接并发处理
	const connConcurrency = 3
	sem := make(chan struct{}, connConcurrency)

	for connID, connMappings := range byConn {
		wg.Add(1)
		sem <- struct{}{}
		go func(connID uint, connMappings []models.ProductMapping) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := s.syncConnectionStock(connID, connMappings); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				logger.Warnw("sync_connection_stock_failed", "connection_id", connID, "error", err)
			}
		}(connID, connMappings)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// fullSyncInterval 强制全量同步间隔：超过此时长后下次同步必走全量，
// 用于发现上游下架/删除（增量模式下这些商品不会再次出现在 updated_after 之后的列表里）
const fullSyncInterval = 24 * time.Hour

// syncConnectionStock 按连接批量同步：一次 ListProducts 拉取所有商品，内存匹配映射
func (s *ProductMappingService) syncConnectionStock(connectionID uint, connMappings []models.ProductMapping) error {
	conn, err := s.connService.GetByID(connectionID)
	if err != nil || conn == nil {
		return fmt.Errorf("get connection %d: %w", connectionID, err)
	}

	adapter, err := s.connService.GetAdapter(conn)
	if err != nil {
		return fmt.Errorf("get adapter for connection %d: %w", connectionID, err)
	}

	// 读取上次同步时间用于增量同步
	syncCtx := context.Background()
	lastSyncKey := fmt.Sprintf("upstream:last_sync:%d", connectionID)
	lastFullSyncKey := fmt.Sprintf("upstream:last_full_sync:%d", connectionID)
	var updatedAfter *time.Time
	if lastSyncStr, err := cache.GetString(syncCtx, lastSyncKey); err == nil && lastSyncStr != "" {
		if t, err := time.Parse(time.RFC3339, lastSyncStr); err == nil {
			// 往前推 1 分钟作为安全窗口
			safeTime := t.Add(-1 * time.Minute)
			updatedAfter = &safeTime
		}
	}

	// 距离上次全量超过阈值则强制走全量，用于发现上游下架/删除
	if updatedAfter != nil {
		if lastFullStr, err := cache.GetString(syncCtx, lastFullSyncKey); err == nil && lastFullStr != "" {
			if t, parseErr := time.Parse(time.RFC3339, lastFullStr); parseErr == nil {
				if time.Since(t) >= fullSyncInterval {
					logger.Infow("sync_force_full", "connection_id", connectionID, "last_full_sync", t)
					updatedAfter = nil
				}
			}
		} else {
			// 从未记录过全量时间 → 本次走全量
			updatedAfter = nil
		}
	}

	syncStartTime := time.Now()

	// 批量拉取上游商品（分页）。include_inactive=true 让上游连同已下架商品一起返回，
	// 下游凭此识别"上游已下架"和"上游已删除"两种状态。
	upstreamProducts := make(map[uint]upstream.UpstreamProduct)
	includesInactive := false
	page := 1
	const pageSize = 50
	for {
		ctx, cancel := context.WithTimeout(syncCtx, 30*time.Second)
		result, err := adapter.ListProducts(ctx, upstream.ListProductsOpts{
			Page:            page,
			PageSize:        pageSize,
			UpdatedAfter:    updatedAfter,
			IncludeInactive: true,
		})
		cancel()
		if err != nil {
			// 增量拉取失败时回退到全量
			if updatedAfter != nil {
				logger.Warnw("sync_incremental_failed_fallback_full", "connection_id", connectionID, "error", err)
				updatedAfter = nil
				page = 1
				upstreamProducts = make(map[uint]upstream.UpstreamProduct)
				continue
			}
			return fmt.Errorf("list upstream products page %d: %w", page, err)
		}

		// 上游回声字段：旧版上游不识别 include_inactive，会返回 false
		if page == 1 {
			includesInactive = result.IncludesInactive
		}

		for _, p := range result.Items {
			upstreamProducts[p.ID] = p
		}

		if len(upstreamProducts) >= result.Total || len(result.Items) == 0 {
			break
		}
		page++
		if page > 200 { // 安全限制
			break
		}
	}

	// 如果是增量同步且无更新，跳过
	if updatedAfter != nil && len(upstreamProducts) == 0 {
		logger.Debugw("sync_skip_no_updates", "connection_id", connectionID)
		// 仍然更新时间戳
		_ = cache.SetString(syncCtx, lastSyncKey, syncStartTime.Format(time.RFC3339), 48*time.Hour)
		return nil
	}

	// 对每个映射执行同步
	now := time.Now()
	isFullSync := updatedAfter == nil
	for i := range connMappings {
		mapping := &connMappings[i]
		upProduct, ok := upstreamProducts[mapping.UpstreamProductID]
		if !ok {
			if !isFullSync {
				// 增量模式下未返回说明没有变化，跳过
				continue
			}
			// 全量模式 + 上游真实支持 include_inactive：下架商品也应在列表中，
			// 仍然 missing 即说明上游已软删除。
			if includesInactive {
				_ = s.markUpstreamUnavailable(mapping, models.UpstreamStatusDeleted, now)
				continue
			}
			// 旧上游不支持 include_inactive，无法区分"下架"和"删除"，
			// 仅打日志告警避免误下架（管理员可手动同步触发判定）。
			logger.Warnw("sync_upstream_product_missing_legacy",
				"connection_id", connectionID,
				"upstream_product_id", mapping.UpstreamProductID,
				"local_product_id", mapping.LocalProductID,
			)
			continue
		}
		// 上游 is_active=false → 标记为 inactive
		if !upProduct.IsActive {
			_ = s.markUpstreamUnavailable(mapping, models.UpstreamStatusInactive, now)
			continue
		}
		s.syncProductFromData(mapping, conn, &upProduct, &now)
	}

	// 记录本次同步时间
	_ = cache.SetString(syncCtx, lastSyncKey, syncStartTime.Format(time.RFC3339), 48*time.Hour)
	if isFullSync {
		_ = cache.SetString(syncCtx, lastFullSyncKey, syncStartTime.Format(time.RFC3339), 7*24*time.Hour)
	}

	logger.Infow("sync_connection_stock_done",
		"connection_id", connectionID,
		"mappings", len(connMappings),
		"upstream_fetched", len(upstreamProducts),
		"incremental", !isFullSync,
		"includes_inactive", includesInactive,
	)
	return nil
}

// syncProductFromData 使用已拉取的上游数据同步单个映射（不再发 HTTP 请求）
// 调用方应保证 upProduct.IsActive == true（下架/删除分支由 caller 处理）
func (s *ProductMappingService) syncProductFromData(mapping *models.ProductMapping, conn *models.SiteConnection, upProduct *upstream.UpstreamProduct, now *time.Time) {
	// ── 1. 同步本地商品字段 ──
	localProduct, err := s.productRepo.GetByID(strconv.FormatUint(uint64(mapping.LocalProductID), 10))
	if err != nil || localProduct == nil {
		return
	}

	if upProduct.ManualFormSchema != nil {
		localProduct.ManualFormSchemaJSON = upProduct.ManualFormSchema
		_ = s.productRepo.Update(localProduct)
	}

	// ── 2. 同步 SKU ──
	skuMappings, err := s.skuMappingRepo.ListByProductMapping(mapping.ID)
	if err != nil {
		return
	}

	upstreamSKUMap := make(map[uint]upstream.UpstreamSKU, len(upProduct.SKUs))
	for _, us := range upProduct.SKUs {
		upstreamSKUMap[us.ID] = us
	}

	existingByUpstreamID := make(map[uint]*models.SKUMapping, len(skuMappings))
	for i := range skuMappings {
		existingByUpstreamID[skuMappings[i].UpstreamSKUID] = &skuMappings[i]
	}

	// 更新已有映射
	for i := range skuMappings {
		upSKU, ok := upstreamSKUMap[skuMappings[i].UpstreamSKUID]
		if !ok {
			skuMappings[i].UpstreamIsActive = false
			skuMappings[i].UpstreamStock = 0
			skuMappings[i].StockSyncedAt = now
			_ = s.skuMappingRepo.Update(&skuMappings[i])
			localSKU, _ := s.productSKURepo.GetByID(skuMappings[i].LocalSKUID)
			if localSKU != nil && localSKU.IsActive {
				localSKU.IsActive = false
				_ = s.productSKURepo.Update(localSKU)
			}
			continue
		}

		upPrice, priceErr := decimal.NewFromString(upSKU.PriceAmount)
		if priceErr != nil {
			logger.Warnw("sync_sku_price_parse_error",
				"upstream_sku_id", upSKU.ID,
				"price_amount", upSKU.PriceAmount,
				"error", priceErr,
			)
			// 仅同步库存状态，跳过价格更新
			skuMappings[i].UpstreamIsActive = upSKU.IsActive
			skuMappings[i].StockSyncedAt = now
			skuMappings[i].UpstreamStock = upSKU.StockQuantity
			_ = s.skuMappingRepo.Update(&skuMappings[i])
			continue
		}
		skuMappings[i].UpstreamPrice = models.NewMoneyFromDecimal(upPrice.Round(2))
		skuMappings[i].UpstreamIsActive = upSKU.IsActive
		skuMappings[i].StockSyncedAt = now
		skuMappings[i].UpstreamStock = upSKU.StockQuantity
		_ = s.skuMappingRepo.Update(&skuMappings[i])

		localSKU, _ := s.productSKURepo.GetByID(skuMappings[i].LocalSKUID)
		if localSKU != nil {
			localSKU.SpecValuesJSON = upSKU.SpecValues
			localSKU.IsActive = upSKU.IsActive
			if conn.AutoSyncPrice {
				newLocalPrice := CalculateLocalPrice(upPrice, conn.ExchangeRate, conn.PriceMarkupPercent, conn.PriceRoundingMode)
				localSKU.PriceAmount = models.NewMoneyFromDecimal(newLocalPrice.Round(2))
				localSKU.CostPriceAmount = models.NewMoneyFromDecimal(convertCurrency(upPrice, conn.ExchangeRate).Round(2))
			}
			_ = s.productSKURepo.Update(localSKU)
		}
	}

	// 上游新增 SKU
	for _, upSKU := range upProduct.SKUs {
		if _, exists := existingByUpstreamID[upSKU.ID]; exists {
			continue
		}
		skuPrice, priceErr := decimal.NewFromString(upSKU.PriceAmount)
		if priceErr != nil {
			logger.Warnw("sync_new_sku_price_parse_error",
				"upstream_sku_id", upSKU.ID,
				"price_amount", upSKU.PriceAmount,
				"error", priceErr,
			)
			continue
		}
		localPrice := CalculateLocalPrice(skuPrice, conn.ExchangeRate, conn.PriceMarkupPercent, conn.PriceRoundingMode)
		newLocalSKU := models.ProductSKU{
			ProductID:       mapping.LocalProductID,
			SKUCode:         upSKU.SKUCode,
			SpecValuesJSON:  upSKU.SpecValues,
			PriceAmount:     models.NewMoneyFromDecimal(localPrice.Round(2)),
			CostPriceAmount: models.NewMoneyFromDecimal(convertCurrency(skuPrice, conn.ExchangeRate).Round(2)),
			IsActive:        upSKU.IsActive,
			SortOrder:       0,
		}
		if err := s.productSKURepo.Create(&newLocalSKU); err != nil {
			continue
		}
		newSKUMapping := &models.SKUMapping{
			ProductMappingID: mapping.ID,
			LocalSKUID:       newLocalSKU.ID,
			UpstreamSKUID:    upSKU.ID,
			UpstreamPrice:    models.NewMoneyFromDecimal(skuPrice.Round(2)),
			UpstreamIsActive: upSKU.IsActive,
			UpstreamStock:    upSKU.StockQuantity,
			StockSyncedAt:    now,
		}
		_ = s.skuMappingRepo.Create(newSKUMapping)
	}

	// 同步价格
	if conn.AutoSyncPrice && localProduct != nil {
		s.recalcProductPrice(localProduct)
	}

	// ── 3. 更新映射记录（同时把状态从 inactive/deleted 恢复为 active）──
	upFulfillment := upProduct.FulfillmentType
	if upFulfillment != constants.FulfillmentTypeAuto {
		upFulfillment = constants.FulfillmentTypeManual
	}
	mapping.UpstreamFulfillmentType = upFulfillment
	mapping.UpstreamStatus = models.UpstreamStatusActive
	mapping.LastSyncedAt = now
	_ = s.mappingRepo.Update(mapping)
}

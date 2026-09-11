package handler

import (
	"context"
	"errors"
	"strconv"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/hlog"
	"github.com/cloudwego/hertz/pkg/common/utils"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"gorm.io/gorm"

	"k_cockpit/internal/model"
)

// maxPageSize 是分页接口允许的最大每页条数，防止单次请求拉取过多数据占用内存。
const maxPageSize = 100

// User 提供用户资源的示例接口。
//
// 依赖通过构造函数注入而非全局变量，便于测试时替换为测试数据库。
type User struct {
	db *gorm.DB
}

// NewUser 创建 User handler。
func NewUser(db *gorm.DB) *User {
	return &User{db: db}
}

// createUserRequest 是创建用户的请求体。
// `required` 由 Hertz 在绑定阶段校验，缺失时 Bind 会返回错误。
type createUserRequest struct {
	Name  string `json:"name,required"`
	Email string `json:"email,required"`
}

// Create 处理 POST /api/v1/users。
func (h *User) Create(ctx context.Context, c *app.RequestContext) {
	var req createUserRequest
	if err := c.Bind(&req); err != nil {
		c.JSON(consts.StatusBadRequest, utils.H{"error": "参数不合法"})
		return
	}

	user := model.User{Name: req.Name, Email: req.Email}
	if err := h.db.WithContext(ctx).Create(&user).Error; err != nil {
		// 具体原因记录到日志，响应中不暴露内部细节。
		hlog.Errorf("创建用户失败: %v", err)
		c.JSON(consts.StatusInternalServerError, utils.H{"error": "创建用户失败"})
		return
	}

	c.JSON(consts.StatusCreated, user)
}

// Get 处理 GET /api/v1/users/:id。
func (h *User) Get(ctx context.Context, c *app.RequestContext) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(consts.StatusBadRequest, utils.H{"error": "id 必须是正整数"})
		return
	}

	var user model.User
	if err := h.db.WithContext(ctx).First(&user, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(consts.StatusNotFound, utils.H{"error": "用户不存在"})
			return
		}
		hlog.Errorf("查询用户失败: %v", err)
		c.JSON(consts.StatusInternalServerError, utils.H{"error": "查询用户失败"})
		return
	}

	c.JSON(consts.StatusOK, user)
}

// List 处理 GET /api/v1/users，支持 page 与 page_size 分页。
func (h *User) List(ctx context.Context, c *app.RequestContext) {
	page, _ := strconv.Atoi(c.Query("page"))
	if page < 1 {
		page = 1
	}

	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	if pageSize < 1 || pageSize > maxPageSize {
		pageSize = 20
	}

	var (
		users []model.User
		total int64
	)

	// 复用同一条查询构建器，Count 与 Find 各自执行一次即可，避免重复组装条件。
	query := h.db.WithContext(ctx).Model(&model.User{})
	if err := query.Count(&total).Error; err != nil {
		hlog.Errorf("统计用户总数失败: %v", err)
		c.JSON(consts.StatusInternalServerError, utils.H{"error": "查询用户失败"})
		return
	}

	if err := query.
		Order("id DESC").
		Limit(pageSize).
		Offset((page - 1) * pageSize).
		Find(&users).Error; err != nil {
		hlog.Errorf("查询用户列表失败: %v", err)
		c.JSON(consts.StatusInternalServerError, utils.H{"error": "查询用户失败"})
		return
	}

	c.JSON(consts.StatusOK, utils.H{
		"data": users,
		"pagination": utils.H{
			"page":      page,
			"page_size": pageSize,
			"total":     total,
		},
	})
}

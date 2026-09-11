package handler_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"gorm.io/gorm"

	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/handler"
	"k_cockpit/internal/model"
)

// newTestServer 构造测试引擎，只注册被测路由，避免无关路由干扰。
// 数据库使用临时目录下的 SQLite 文件，每个测试互不影响。
func newTestServer(t *testing.T) (*server.Hertz, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "test.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("初始化测试数据库失败: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}

	h := server.Default()

	users := handler.NewUser(db)
	h.POST("/api/v1/users", users.Create)
	h.GET("/api/v1/users", users.List)
	h.GET("/api/v1/users/:id", users.Get)
	h.GET("/health", handler.Health(db))

	return h, db
}

func jsonBody(t *testing.T, v any) *ut.Body {
	t.Helper()

	payload, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("编码请求体失败: %v", err)
	}
	return &ut.Body{Body: bytes.NewReader(payload), Len: len(payload)}
}

func jsonHeader() ut.Header {
	return ut.Header{Key: "Content-Type", Value: "application/json"}
}

func TestUser_Create(t *testing.T) {
	h, db := newTestServer(t)

	w := ut.PerformRequest(h.Engine, "POST", "/api/v1/users",
		jsonBody(t, map[string]string{"name": "张三", "email": "zhangsan@example.com"}),
		jsonHeader(),
	)

	if w.Code != consts.StatusCreated {
		t.Fatalf("状态码 = %d, 期望 %d, 响应: %s", w.Code, consts.StatusCreated, w.Body.String())
	}

	var created model.User
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if created.ID == 0 {
		t.Error("响应中的 ID 不应为 0")
	}

	// 确认数据确实写入数据库，而非仅回显请求。
	var count int64
	if err := db.Model(&model.User{}).Where("email = ?", "zhangsan@example.com").Count(&count).Error; err != nil {
		t.Fatalf("查询数据库失败: %v", err)
	}
	if count != 1 {
		t.Errorf("数据库中的用户数 = %d, 期望 1", count)
	}
}

func TestUser_Create_缺少必填字段(t *testing.T) {
	h, _ := newTestServer(t)

	w := ut.PerformRequest(h.Engine, "POST", "/api/v1/users",
		jsonBody(t, map[string]string{"email": "no-name@example.com"}),
		jsonHeader(),
	)

	if w.Code != consts.StatusBadRequest {
		t.Fatalf("状态码 = %d, 期望 %d", w.Code, consts.StatusBadRequest)
	}
}

func TestUser_Get(t *testing.T) {
	h, db := newTestServer(t)

	user := model.User{Name: "李四", Email: "lisi@example.com"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("准备测试数据失败: %v", err)
	}

	w := ut.PerformRequest(h.Engine, "GET", fmt.Sprintf("/api/v1/users/%d", user.ID), nil)

	if w.Code != consts.StatusOK {
		t.Fatalf("状态码 = %d, 期望 %d", w.Code, consts.StatusOK)
	}

	var got model.User
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if got.ID != user.ID || got.Email != user.Email {
		t.Errorf("返回数据 = %+v, 期望 ID=%d Email=%s", got, user.ID, user.Email)
	}
}

func TestUser_Get_不存在(t *testing.T) {
	h, _ := newTestServer(t)

	w := ut.PerformRequest(h.Engine, "GET", "/api/v1/users/99999", nil)

	if w.Code != consts.StatusNotFound {
		t.Fatalf("状态码 = %d, 期望 %d", w.Code, consts.StatusNotFound)
	}
}

func TestUser_Get_id非法(t *testing.T) {
	h, _ := newTestServer(t)

	w := ut.PerformRequest(h.Engine, "GET", "/api/v1/users/abc", nil)

	if w.Code != consts.StatusBadRequest {
		t.Fatalf("状态码 = %d, 期望 %d", w.Code, consts.StatusBadRequest)
	}
}

func TestUser_List_分页(t *testing.T) {
	h, db := newTestServer(t)

	for i := 1; i <= 3; i++ {
		user := model.User{
			Name:  fmt.Sprintf("user-%d", i),
			Email: fmt.Sprintf("user%d@example.com", i),
		}
		if err := db.Create(&user).Error; err != nil {
			t.Fatalf("准备测试数据失败: %v", err)
		}
	}

	w := ut.PerformRequest(h.Engine, "GET", "/api/v1/users?page=1&page_size=2", nil)
	if w.Code != consts.StatusOK {
		t.Fatalf("状态码 = %d, 期望 %d", w.Code, consts.StatusOK)
	}

	var resp struct {
		Data       []model.User `json:"data"`
		Pagination struct {
			Page     int   `json:"page"`
			PageSize int   `json:"page_size"`
			Total    int64 `json:"total"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	if len(resp.Data) != 2 {
		t.Errorf("本页返回 %d 条, 期望 2 条", len(resp.Data))
	}
	if resp.Pagination.Total != 3 {
		t.Errorf("总数 = %d, 期望 3", resp.Pagination.Total)
	}
}

func TestUser_List_page_size超上限时回落默认值(t *testing.T) {
	h, _ := newTestServer(t)

	// page_size 超过上限时应回落为默认值 20，而不是按 9999 查询。
	w := ut.PerformRequest(h.Engine, "GET", "/api/v1/users?page=1&page_size=9999", nil)
	if w.Code != consts.StatusOK {
		t.Fatalf("状态码 = %d, 期望 %d", w.Code, consts.StatusOK)
	}

	var resp struct {
		Pagination struct {
			PageSize int `json:"page_size"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Pagination.PageSize != 20 {
		t.Errorf("page_size = %d, 期望回落到 20", resp.Pagination.PageSize)
	}
}

// Package headers собирает три класса заголовков апстрима (common / chat / billing / refresh).
// Правила — из docs/api-reference.md §0/§4/§6.
package upstream

import (
	"net/http"

	"workbuddy2api/internal/auth"
)

const (
	clientUA        = "CLI/2.63.2 CodeBuddy/2.63.2"
	originRefererCN = "https://www.codebuddy.cn"
)

func originRefererFor(a *auth.Auth) string {
	return originRefererCN
}

// userAgent возвращает текущий исходящий UA: если Client.UserAgent непуст — перекрывает (действует на все исходящие запросы),
// пусто = сохраняем clientUA как есть. Из соображений очистки отпечатка: значение по умолчанию не меняем, переписываем только при явной настройке пользователем.
func (c *Client) userAgent() string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	return clientUA
}

// CommonHeaders выставляет общие для всех API заголовки.
func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.userAgent())
}

// ChatHeaders добавляет поверх common специфичные для chat заголовки аккаунта.
// Отсутствующие поля — по соглашению X-No-* (как в официальном CLI CodeBuddy).
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	if a.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
// Красная линия безопасности: никогда не несём X-Refresh-Token в chat-запросах.
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	req.Header.Set("X-Product", "SaaS")
}

// BillingHeaders — заголовки billing-интерфейса.
// Семантика UA: по умолчанию **не выставляем** (сохраняем как есть, у Go-клиента свой UA по умолчанию); перекрываем только при явной настройке
// непустого c.UserAgent — чтобы путь по умолчанию не вносил в billing новый отпечаток UA.
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if c != nil && c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}
}

// RefreshHeaders — заголовки refresh-эндпоинта (X-Refresh-Token разрешён только здесь).
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
}

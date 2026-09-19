package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/model"
	"github.com/stretchr/testify/require"
)

func newsletterRequest(handler gin.HandlerFunc, method, url, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, url, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler(c)
	return w
}

func TestNewsletterSubscribePersistsConsentAndDeduplicates(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.NewsletterSubscriber{}))
	first := newsletterRequest(SubscribeNewsletter, "POST", "/", `{"email":" Reader@Example.com ","language":"zh","consent":true}`)
	require.Equal(t, http.StatusOK, first.Code)
	second := newsletterRequest(SubscribeNewsletter, "POST", "/", `{"email":"reader@example.com","language":"en","consent":true}`)
	require.Equal(t, first.Body.String(), second.Body.String())
	var rows []model.NewsletterSubscriber
	require.NoError(t, db.Find(&rows).Error)
	require.Len(t, rows, 1)
	require.Equal(t, "reader@example.com", rows[0].Email)
	require.Equal(t, "zh", rows[0].Language)
	require.False(t, rows[0].CreatedAt.IsZero())
}

func TestNewsletterRejectsInvalidInputWithoutSaving(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.NewsletterSubscriber{}))
	for _, body := range []string{
		`{`, `{}`, `{"email":"reader@example.com"}`,
		`{"email":"invalid","consent":true}`,
		`{"email":"Name <reader@example.com>","consent":true}`,
		`{"email":"one@example.com;two@example.com","consent":true}`,
		`{"email":"reader@example.com\r\nBcc: other@example.com","consent":true}`,
		`{"email":"` + strings.Repeat("a", 255) + `@example.com","consent":true}`,
		`{"email":"reader@example.com","consent":true,"extra":"` + strings.Repeat("x", 4096) + `"}`,
	} {
		w := newsletterRequest(SubscribeNewsletter, "POST", "/", body)
		require.Equal(t, http.StatusBadRequest, w.Code)
	}
	var count int64
	require.NoError(t, db.Model(&model.NewsletterSubscriber{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestNewsletterStorageFailureIsNotReportedAsSuccess(t *testing.T) {
	setupTestDB(t) // Deliberately omit the newsletter table.
	w := newsletterRequest(SubscribeNewsletter, "POST", "/", `{"email":"reader@example.com","consent":true}`)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Contains(t, w.Body.String(), `"success":false`)
	require.NotContains(t, w.Body.String(), "no such table")
}

func TestNewsletterListPagination(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.NewsletterSubscriber{}))
	require.NoError(t, model.SubscribeNewsletter("first@example.com", "zh"))
	require.NoError(t, model.SubscribeNewsletter("second@example.com", "invalid"))
	w := newsletterRequest(GetNewsletterSubscribers, "GET", "/?page=1&page_size=1", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"total":2`)
	require.Contains(t, w.Body.String(), `"language":"en"`)
	require.Contains(t, w.Body.String(), "second@example.com")
	require.NotContains(t, w.Body.String(), "first@example.com")
	for _, query := range []string{"page=0", "page=-1", "page=invalid", "page_size=0", "page_size=101"} {
		w = newsletterRequest(GetNewsletterSubscribers, "GET", "/?"+query, "")
		require.Equal(t, http.StatusBadRequest, w.Code)
	}
}

package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// LoginStart 是应用层使用的协议登录结果，不暴露内部 SRP 与挑战会话结构。
type LoginStart struct {
	Needs2FA  bool           `json:"needs_2fa"`
	PendingID string         `json:"pending_id,omitempty"`
	AppleID   string         `json:"apple_id"`
	ExpiresAt string         `json:"expires_at,omitempty"`
	Message   string         `json:"message"`
	Session   *ICloudSession `json:"session,omitempty"`
}

// Broker 将已验证的 Apple 协议实现封装为应用服务调用的稳定边界。
type Broker struct {
	iCloudWeb   *appleAuthPendingStore
	appleManage *appleAuthPendingStore
}

func NewBroker() *Broker {
	return &Broker{iCloudWeb: newAppleAuthPendingStore(), appleManage: newAppleAuthPendingStore()}
}

func (b *Broker) StartLogin(ctx context.Context, kind, appleID, password, twoFactorMethod string) (LoginStart, error) {
	var result appleAuthStartResult
	var err error
	switch strings.TrimSpace(kind) {
	case LoginStateAppleAccount:
		result, err = NewAppleAuthClient().StartAppleAccountManageLogin(ctx, appleID, password, b.appleManage, twoFactorMethod)
	case LoginStateICloudWeb:
		result, err = NewAppleAuthClient().StartLogin(ctx, appleID, password, "www.icloud.com.cn", defaultAppleOAuthClientID, b.iCloudWeb, twoFactorMethod)
	default:
		return LoginStart{}, fmt.Errorf("不支持的 Apple 登录类型")
	}
	if err != nil {
		return LoginStart{}, err
	}
	out := LoginStart{Needs2FA: result.Needs2FA, PendingID: result.PendingID, AppleID: result.AppleID, Message: result.Message}
	if !result.ExpiresAt.IsZero() {
		out.ExpiresAt = result.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if !result.Needs2FA {
		session := result.Session
		out.Session = &session
	}
	return out, nil
}

func (b *Broker) Verify2FA(ctx context.Context, kind, pendingID, code string, phoneNumber json.RawMessage) (ICloudSession, error) {
	switch strings.TrimSpace(kind) {
	case LoginStateAppleAccount:
		pending, ok := b.appleManage.get(pendingID)
		if !ok {
			return ICloudSession{}, fmt.Errorf("登录挑战已过期，请重新登录")
		}
		session, err := NewAppleAuthClient().SubmitAppleAccountManage2FA(ctx, pending, code, phoneNumber)
		if err == nil {
			b.appleManage.delete(pendingID)
		}
		return session, err
	case LoginStateICloudWeb:
		pending, ok := b.iCloudWeb.get(pendingID)
		if !ok {
			return ICloudSession{}, fmt.Errorf("登录挑战已过期，请重新登录")
		}
		session, err := NewAppleAuthClient().Submit2FA(ctx, pending, code)
		if err == nil {
			b.iCloudWeb.delete(pendingID)
		}
		return session, err
	default:
		return ICloudSession{}, fmt.Errorf("不支持的 Apple 登录类型")
	}
}

func CheckConnection(ctx context.Context, kind, secretJSON string) error {
	switch strings.TrimSpace(kind) {
	case LoginStateICloudIMAP:
		var secret struct {
			Email       string `json:"email"`
			AppPassword string `json:"app_password"`
		}
		if err := json.Unmarshal([]byte(secretJSON), &secret); err != nil {
			return err
		}
		return CheckICloudIMAPLogin(ctx, secret.Email, secret.AppPassword)
	case LoginStateICloudWeb, LoginStateAppleAccount:
		_, _, err := RefreshConnection(ctx, kind, secretJSON)
		return err
	default:
		return fmt.Errorf("不支持的连接类型")
	}
}

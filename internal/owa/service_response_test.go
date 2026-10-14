package owa

import (
	"context"
	"strings"
	"testing"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func TestOWAServiceRejectsNativeErrorsWithoutMessageText(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response any
	}{
		{"single", okBody(map[string]any{"ResponseMessages": map[string]any{"Items": []any{map[string]any{"ResponseClass": "Error", "ResponseCode": "ErrorInvalidPropertySet"}}}})},
		{"multi partial success", okBody(map[string]any{"ResponseMessages": map[string]any{"Items": []any{map[string]any{"ResponseClass": "Success", "ResponseCode": "NoError", "Items": []any{map[string]any{"ItemId": map[string]any{"Id": "partial"}}}}, map[string]any{"ResponseClass": "Error", "ResponseCode": "ErrorInvalidPropertySet"}}}})},
		{"unwrapped", okBody(map[string]any{"ResponseClass": "Error", "ResponseCode": "ErrorInvalidPropertySet"})},
		{"code without class", okBody(map[string]any{"ResponseCode": "ErrorInvalidPropertySet"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, p := newFake(t, func(string, map[string]any) any { return tc.response })
			result, err := p.(*provider).call(context.Background(), "GetItem", map[string]any{})
			if err == nil || !strings.Contains(err.Error(), "ErrorInvalidPropertySet") || result != nil {
				t.Fatalf("native error became success: result=%+v err=%v", result, err)
			}
		})
	}
}

func TestOWATaskMutationPropagatesCodeOnlyNativeErrors(t *testing.T) {
	for _, action := range []string{"UpdateItem", "DeleteItem"} {
		t.Run(action, func(t *testing.T) {
			_, p := newFake(t, func(got string, _ map[string]any) any {
				if got != action {
					t.Errorf("action=%s want %s", got, action)
				}
				return okBody(map[string]any{"ResponseMessages": map[string]any{"Items": []any{map[string]any{"ResponseClass": "Error", "ResponseCode": "ErrorInvalidPropertySet"}}}})
			})
			var err error
			if action == "UpdateItem" {
				err = p.UpdateTask(context.Background(), bridge.Task{ID: "cloud", UID: "client", Title: "Do it"})
			} else {
				err = p.DeleteTask(context.Background(), "tasks", "cloud")
			}
			if err == nil || !strings.Contains(err.Error(), "ErrorInvalidPropertySet") {
				t.Fatalf("task mutation falsely succeeded: %v", err)
			}
		})
	}
}

func TestOWAServiceKeepsNominalResponseShapes(t *testing.T) {
	for _, count := range []int{1, 2} {
		var messages []any
		for i := 0; i < count; i++ {
			messages = append(messages, map[string]any{"ResponseClass": "Success", "ResponseCode": "NoError", "Items": []any{map[string]any{"Subject": "ok"}}})
		}
		_, p := newFake(t, func(string, map[string]any) any {
			return okBody(map[string]any{"ResponseMessages": map[string]any{"Items": messages}})
		})
		result, err := p.(*provider).call(context.Background(), "GetItem", map[string]any{})
		if err != nil || len(allItems(result)) != count {
			t.Fatalf("nominal service result=%+v err=%v", result, err)
		}
		if count == 1 && len(arr(result, "ResponseMessages", "Items")) != 0 {
			t.Fatal("single result no longer unwrapped")
		}
	}
}

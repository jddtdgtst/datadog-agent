// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023-present Datadog, Inc.

package windows

import (
	"fmt"
	"strings"

	"github.com/DataDog/datadog-agent/test/new-e2e/pkg/utils/e2e/client"
)

// GetProductCodeByName returns the product code GUID for the given product name
func GetProductCodeByName(client client.VM, name string) (string, error) {
	// Read from registry instead of using Win32_Product, which has negative side effects
	// https://gregramsey.net/2012/02/20/win32_product-is-evil/
	cmd := fmt.Sprintf(`(@(Get-ChildItem -Path "HKLM:SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall" -Recurse ; Get-ChildItem -Path "HKLM:SOFTWARE\Wow6432Node\Microsoft\Windows\CurrentVersion\Uninstall" -Recurse ) | Where {$_.GetValue("DisplayName") -like "%s" }).PSChildName`, name)
	val, err := client.ExecuteWithError(cmd)
	if err != nil {
		fmt.Println(val)
		return "", err
	}
	return strings.TrimSpace(val), nil
}
package management

const templateScriptShellCore = `        const LANG_STORAGE_KEY = "antigravity_priority_lang";
        let currentLang = "zh-CN";
        try {
            const savedLang = localStorage.getItem(LANG_STORAGE_KEY);
            if (savedLang === "zh-CN" || savedLang === "en-US") {
                currentLang = savedLang;
            }
        } catch (_) {}
        let activeTab = "overview";
        let countdownInterval = null;
        let dashboardRefreshInterval = null;
        let silentDashboardRefreshInFlight = false;
        let probeCooldownTimer = null;
        let scheduleConfig = null;
        let dynamicConfig = null;
		let managementKey = "";

        function getManagementKey() {
			return managementKey;
        }

        function setSavedKey(key) {
			managementKey = key ? key.trim() : "";
        }

        function openKeyModal() {
            const input = document.getElementById("manualKeyInput");
            if (input) input.value = getManagementKey();
            const modal = document.getElementById("keyModal");
            if (modal) modal.hidden = false;
        }

        function closeKeyModal() {
            const modal = document.getElementById("keyModal");
            if (modal) modal.hidden = true;
        }

        function saveKeyAndRefresh() {
            const input = document.getElementById("manualKeyInput");
            if (input) {
                setSavedKey(input.value);
            }
            closeKeyModal();
            refreshDashboard();
        }

        const THEME_STORAGE_KEY = "antigravity_priority_theme";

        function updateThemeIcon(theme) {
            const icon = document.getElementById("themeIcon");
            if (!icon) return;
            if (theme === "dark") {
                icon.textContent = "🌙";
            } else if (theme === "light") {
                icon.textContent = "☀️";
            } else {
                icon.textContent = "🌓";
            }
        }

        function toggleTheme() {
            const currentTheme = document.documentElement.getAttribute("data-theme");
            let nextTheme = "light";
            if (!currentTheme) {
                const isDark = window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches;
                nextTheme = isDark ? "light" : "dark";
            } else {
                nextTheme = currentTheme === "dark" ? "light" : "dark";
            }
            document.documentElement.setAttribute("data-theme", nextTheme);
            try {
                localStorage.setItem(THEME_STORAGE_KEY, nextTheme);
            } catch (_) {}
            updateThemeIcon(nextTheme);
        }

        function syncThemeFromParent() {
            try {
                if (window.parent && window.parent !== window && window.parent.document && window.parent.document.documentElement) {
                    const pDoc = window.parent.document.documentElement;
                    const pBody = window.parent.document.body;

                    const pTheme = pDoc.getAttribute("data-theme") || (pBody && pBody.getAttribute("data-theme"));
                    if (pTheme) {
                        document.documentElement.setAttribute("data-theme", pTheme);
                        updateThemeIcon(pTheme);
                    } else {
                        document.documentElement.removeAttribute("data-theme");
                        updateThemeIcon("system");
                    }

                    const isDark = pDoc.classList.contains("dark") || (pBody && pBody.classList.contains("dark")) || pTheme === "dark";
                    if (isDark) {
                        document.documentElement.setAttribute("data-theme", "dark");
                        updateThemeIcon("dark");
                    }

                    const parentStyle = window.parent.getComputedStyle(pDoc);
                    const cpaVarNames = [
                        '--bg-primary', '--bg-secondary', '--bg-tertiary', '--bg-quinary', '--bg-hover',
                        '--text-primary', '--text-secondary', '--text-tertiary', '--text-muted',
                        '--border-color', '--border-primary', '--border-secondary', '--border-hover',
                        '--primary-color', '--primary-hover', '--primary-active', '--primary-contrast',
                        '--success-color', '--warning-color', '--error-color', '--danger-color',
                        '--amber-color', '--quota-medium-color', '--floating-surface'
                    ];

                    for (const name of cpaVarNames) {
                        const val = parentStyle.getPropertyValue(name);
                        if (val && val.trim()) {
                            document.documentElement.style.setProperty(name, val.trim());
                        }
                    }

                    const sec = parentStyle.getPropertyValue('--bg-secondary') || parentStyle.getPropertyValue('--bg-primary');
                    const tert = parentStyle.getPropertyValue('--bg-tertiary');
                    if (sec && sec.trim()) {
                        document.documentElement.style.setProperty('--bg-surface', sec.trim());
                        document.documentElement.style.setProperty('--bg-card', sec.trim());
                    }
                    if (tert && tert.trim()) {
                        document.documentElement.style.setProperty('--bg-subtle', tert.trim());
                        document.documentElement.style.setProperty('--meter-bg', tert.trim());
                    }
                    return;
                }
            } catch (_) {}

            // Standalone or DevServer mode: restore saved theme
            try {
                const savedTheme = localStorage.getItem(THEME_STORAGE_KEY);
                if (savedTheme === "dark" || savedTheme === "light") {
                    document.documentElement.setAttribute("data-theme", savedTheme);
                    updateThemeIcon(savedTheme);
                } else {
                    const isDark = window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches;
                    updateThemeIcon(isDark ? "dark" : "light");
                }
            } catch (_) {}
        }

        syncThemeFromParent();
        try {
            if (window.parent && window.parent !== window && window.parent.document) {
                const observer = new MutationObserver(function() {
                    syncThemeFromParent();
                });
                observer.observe(window.parent.document.documentElement, {
                    attributes: true,
                    attributeFilter: ["data-theme", "class", "style"]
                });
                if (window.parent.document.body) {
                    observer.observe(window.parent.document.body, {
                        attributes: true,
                        attributeFilter: ["data-theme", "class", "style"]
                    });
                }
            }
        } catch (_) {}

        function t(key) {
            return (I18N[currentLang] && I18N[currentLang][key]) || I18N["zh-CN"][key] || key;
        }

        function isCurrentTimeInScheduleWindow(startStr, endStr) {
            if (!startStr || !endStr) return true;
            var startParts = startStr.trim().split(":");
            var endParts = endStr.trim().split(":");
            if (startParts.length !== 2 || endParts.length !== 2) return true;
            var startMin = parseInt(startParts[0], 10) * 60 + parseInt(startParts[1], 10);
            var endMin = parseInt(endParts[0], 10) * 60 + parseInt(endParts[1], 10);
            if (isNaN(startMin) || isNaN(endMin) || startMin === endMin) return true;
            if (startMin === 0 && endMin === 24 * 60) return true;

            var now = new Date();
            var nowMin = now.getHours() * 60 + now.getMinutes();

            if (startMin < endMin) {
                return nowMin >= startMin && nowMin < endMin;
            }
            // Cross midnight (e.g. 22:00 to 06:00)
            return nowMin >= startMin || nowMin < endMin;
        }

        function toggleLanguage() {
            currentLang = currentLang === "zh-CN" ? "en-US" : "zh-CN";
            try {
                localStorage.setItem(LANG_STORAGE_KEY, currentLang);
            } catch (_) {}
            applyLanguage();
            renderDashboard();
            renderHistory();
            renderDiagnostics();
            renderScheduleStatus();
            if (dynamicConfig) renderDynamicConfigForm(dynamicConfig);
        }

        function applyLanguage() {
            document.documentElement.lang = currentLang;
            document.querySelectorAll("[data-i18n]").forEach(el => {
                const key = el.getAttribute("data-i18n");
                if (key && I18N[currentLang] && I18N[currentLang][key]) {
                    el.innerHTML = I18N[currentLang][key];
                }
            });
            const langLabel = document.getElementById("langLabel");
            if (langLabel) langLabel.textContent = currentLang === "zh-CN" ? "EN / 中文" : "中文 / EN";
            updateAllCustomSelectDisplays();
        }

`

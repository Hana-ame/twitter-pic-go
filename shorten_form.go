package twitter

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// HandleShortenFormV2 是 GET /shorten：返回自包含的前端页面。
//
// 单文件、无构建步骤、无外部 CDN 依赖：直接内联在二进制里由后端吐出，
// 不引入新的静态文件托管路径（server/main.go 里 NoRoute 全交给 gallery SSR，
// 塞静态文件进去会和它抢路由）。
//
// 样式对齐 https://proxy.moonchan.xyz/ （同站既有的「生成器」页）：
// 渐变背景 + 居中白卡 + header 渐变色 + 圆角大阴影。
func HandleShortenFormV2(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(shortenFormHTML))
}

// shortenFormHTML 是自包含页面。
//
// 前端只做两件事：用 new URL() 挡掉明显非法的输入（省一次往返），
// 以及把 POST 的错误码翻成人话。所有真正的校验都在后端
// （validateShortenURL + ouoShortURLRe），前端校验不产生安全性。
const shortenFormHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>短链生成 - moonchan.xyz</title>
<style>
* { margin: 0; padding: 0; box-sizing: border-box; }

body {
    font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto,
                 'Helvetica Neue', Arial, sans-serif;
    background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
    min-height: 100vh;
    padding: 20px;
    color: #333;
}

.container {
    max-width: 720px;
    margin: 0 auto;
    background: white;
    border-radius: 12px;
    box-shadow: 0 20px 60px rgba(0, 0, 0, 0.3);
    overflow: hidden;
}

.header {
    background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
    color: white;
    padding: 30px;
    text-align: center;
}

.header h1 { font-size: 28px; margin-bottom: 10px; }
.header p { opacity: 0.9; font-size: 14px; }

.content { padding: 30px; }

.info-box {
    background: #e7f3ff;
    color: #0b5cad;
    border: 1px solid #b3d7ff;
    border-radius: 8px;
    padding: 12px 16px;
    font-size: 13px;
    margin-bottom: 24px;
    line-height: 1.6;
}

.input-group { margin-bottom: 24px; }

.input-group label {
    display: block;
    font-size: 14px;
    font-weight: 600;
    margin-bottom: 8px;
    color: #333;
}

.input-group input {
    width: 100%;
    padding: 14px 16px;
    font-size: 15px;
    font-family: inherit;
    border: 2px solid #e0e0e0;
    border-radius: 8px;
    outline: none;
    transition: border-color 0.2s;
}

.input-group input:focus { border-color: #667eea; }

.input-group input.invalid { border-color: #dc3545; }

.hint { margin-top: 8px; font-size: 12px; color: #666; }

.btn-generate {
    width: 100%;
    padding: 15px;
    background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
    color: white;
    border: none;
    border-radius: 8px;
    font-size: 16px;
    font-weight: 600;
    cursor: pointer;
    transition: opacity 0.2s, transform 0.1s;
}

.btn-generate:hover:not(:disabled) { transform: translateY(-1px); }
.btn-generate:active:not(:disabled) { transform: translateY(0); }
.btn-generate:disabled { opacity: 0.6; cursor: not-allowed; }

#localError {
    display: none;
    background: #f8d7da;
    color: #721c24;
    border: 1px solid #f5c2c7;
    border-radius: 8px;
    padding: 12px 16px;
    font-size: 13px;
    margin-top: 20px;
}

.results { display: none; margin-top: 24px; }
.results.show { display: block; }

.result-card {
    background: #f8f9fa;
    border: 1px solid #e0e0e0;
    border-radius: 8px;
    padding: 20px;
}

.result-card h3 {
    font-size: 15px;
    color: #333;
    margin-bottom: 14px;
    display: flex;
    align-items: center;
    gap: 8px;
}

.badge {
    background: #28a745;
    color: white;
    font-size: 11px;
    font-weight: 600;
    padding: 2px 8px;
    border-radius: 4px;
}

.url-display {
    background: white;
    border: 1px solid #ddd;
    border-radius: 6px;
    padding: 12px 14px;
    font-family: 'SFMono-Regular', Consolas, 'Liberation Mono', Menlo, monospace;
    font-size: 14px;
    word-break: break-all;
    margin-bottom: 14px;
}

.btn-copy {
    padding: 8px 20px;
    background: #667eea;
    color: white;
    border: none;
    border-radius: 6px;
    font-size: 13px;
    font-weight: 600;
    cursor: pointer;
    transition: background 0.2s;
}

.btn-copy:hover { background: #5568d3; }
.btn-copy.copied { background: #28a745; }

.footer {
    padding: 0 30px 26px;
    font-size: 12px;
    color: #666;
    line-height: 1.7;
}

.footer a { color: #667eea; text-decoration: none; }
</style>
</head>
<body>
<div class="container">
    <div class="header">
        <h1>🔗 短链生成</h1>
        <p>输入长链接，一键生成 ouo.io 短链</p>
    </div>

    <div class="content">
        <div class="info-box">
            💡 短链由 <a href="https://ouo.io" target="_blank" rel="noopener">ouo.io</a>
            生成。访问者点开后会先经过一个广告落地页，这是 ouo.io 的变现方式。
            同一链接重复生成会返回同一条短链。
        </div>

        <form id="shortenForm" autocomplete="off">
            <div class="input-group">
                <label for="targetUrl">目标 URL</label>
                <input type="text" id="targetUrl" name="url"
                       placeholder="例如: https://example.com/very/long/path"
                       required spellcheck="false" autofocus>
                <div class="hint">支持 http / https；留空 scheme 会被当成 http 处理</div>
            </div>

            <button type="submit" class="btn-generate" id="generateBtn">生成短链</button>
        </form>

        <div id="localError"></div>
        <div id="serverError" class="results"></div>
        <div id="results" class="results">
            <div class="result-card">
                <h3>短链 <span class="badge">可复制</span></h3>
                <div class="url-display" id="shortUrl"></div>
                <button type="button" class="btn-copy" id="copyBtn"
                        onclick="copyShortUrl(this)">复制</button>
            </div>
        </div>
    </div>

    <div class="footer">
        服务端接口：<code>POST /api/v2/shorten</code>（{"url": "..."} → {"short_url": "..."}）<br>
        每次生成都会真实调用上游，请勿高频请求。
    </div>
</div>

<script>
(function () {
    'use strict';

    var form = document.getElementById('shortenForm');
    var input = document.getElementById('targetUrl');
    var btn = document.getElementById('generateBtn');
    var results = document.getElementById('results');
    var serverError = document.getElementById('serverError');
    var localError = document.getElementById('localError');
    var shortUrlEl = document.getElementById('shortUrl');

    // 错误码 → 人话。502 三种上游错误必须分开说，否则排查时只知道「坏了」。
    var ERR_TEXT = {
        503: '服务端未配置 OUO_API_KEY，暂时无法生成短链。',
        502: '上游拒绝生成（key 失效、链接格式不支持或响应异常）。请换一个链接试试。',
        504: '上游响应超时（ouo.io 通常要 2-4 秒），请稍后重试。',
        429: '请求过于频繁，请一小时后再试。',
        400: '链接格式不对：必须是完整的 http(s) 地址。'
    };

    function showLocal(msg) {
        localError.textContent = msg;
        localError.style.display = 'block';
        hideResults();
        hideServerError();
        input.classList.add('invalid');
    }

    function showServerError(msg) {
        serverError.innerHTML = '<div class="result-card" style="background:#f8d7da;' +
            'border-color:#f5c2c7;color:#721c24">' + escapeHtml(msg) + '</div>';
        serverError.classList.add('show');
        hideResults();
        localError.style.display = 'none';
    }

    function hideResults() { results.classList.remove('show'); }
    function hideServerError() { serverError.classList.remove('show'); }

    function escapeHtml(s) {
        return String(s).replace(/[&<>"']/g, function (ch) {
            return { '&': '&amp;', '<': '&lt;', '>': '&gt;',
                     '"': '&quot;', "'": '&#39;' }[ch];
        });
    }

    function setBusy(busy) {
        btn.disabled = busy;
        btn.textContent = busy ? '生成中…' : '生成短链';
    }

    // 从错误响应里取出 message，取不到就退回状态码文案。
    function errorMessage(status, data) {
        if (data && typeof data === 'object') {
            if (typeof data.message === 'string' && data.message.length > 0) {
                return data.message;
            }
            if (typeof data.error === 'string' && data.error.length > 0) {
                return data.error;
            }
        }
        return ERR_TEXT[status] || ('请求失败（HTTP ' + status + '）');
    }

    async function submit() {
        var url = input.value.trim();
        localError.style.display = 'none';
        input.classList.remove('invalid');

        if (!url) {
            showLocal('请输入目标 URL。');
            input.focus();
            return;
        }

        // 前端只做一次基本校验，省掉一次往返。后端会再校验一遍。
        try {
            var parsed = new URL(url);
            if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
                showLocal('只支持 http / https 链接。');
                return;
            }
        } catch (e) {
            // 没有 scheme 的输入上游会当 http 处理，这里提示而不是硬拦。
            if (!/^https?:\/\//i.test(url)) {
                showLocal('链接格式不对，请粘贴完整的 http(s) 地址。');
                return;
            }
        }

        setBusy(true);
        try {
            var resp = await fetch('/api/v2/shorten', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ url: url })
            });

            var data = null;
            try { data = await resp.json(); } catch (e) { /* 非 JSON 响应 */ }

            if (!resp.ok) {
                showServerError(errorMessage(resp.status, data));
                return;
            }
            if (!data || !data.short_url) {
                showServerError('上游返回了空短链，请重试。');
                return;
            }

            shortUrlEl.textContent = data.short_url;
            results.classList.add('show');
            hideServerError();
        } catch (e) {
            showServerError('无法连接到服务端，请稍后重试。');
        } finally {
            setBusy(false);
        }
    }

    form.addEventListener('submit', function (e) {
        e.preventDefault();
        submit();
    });

    // copyShortUrl 挂到 window，onclick 属性才能调到。
    window.copyShortUrl = function (btnEl) {
        var text = shortUrlEl.textContent;
        var done = function () {
            var old = btnEl.textContent;
            btnEl.textContent = '已复制';
            btnEl.classList.add('copied');
            setTimeout(function () {
                btnEl.textContent = old;
                btnEl.classList.remove('copied');
            }, 1500);
        };
        var fallback = function () {
            var ta = document.createElement('textarea');
            ta.value = text;
            ta.style.position = 'fixed';
            ta.style.opacity = '0';
            document.body.appendChild(ta);
            ta.select();
            try { document.execCommand('copy'); done(); }
            catch (e) { showLocal('复制失败，请手动选择文字复制。'); }
            document.body.removeChild(ta);
        };
        if (navigator.clipboard && navigator.clipboard.writeText) {
            navigator.clipboard.writeText(text).then(done, fallback);
        } else {
            fallback();
        }
    };
})();
</script>
</body>
</html>
`

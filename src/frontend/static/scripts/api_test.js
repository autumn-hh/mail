document.addEventListener('DOMContentLoaded', () => {
    const authForm = document.getElementById('auth-form');
    const addressInput = document.getElementById('mailbox-address');
    const keyInput = document.getElementById('mailbox-key');
    const authButton = document.getElementById('auth-button');
    const clearSessionButton = document.getElementById('clear-session-button');
    const mailboxInfoButton = document.getElementById('mailbox-info-button');
    const inboxButton = document.getElementById('inbox-button');
    const clearOutputButton = document.getElementById('clear-output-button');
    const authStatus = document.getElementById('auth-status');
    const output = document.getElementById('api-output');

    // 敏感凭据只保存在页面内存，避免调试工具把邮箱密钥或令牌持久化到浏览器。
    const state = {
        address: '',
        accessToken: ''
    };

    function setAuthenticated(authenticated) {
        mailboxInfoButton.disabled = !authenticated;
        inboxButton.disabled = !authenticated;
    }

    function setStatus(kind, message) {
        authStatus.dataset.kind = kind;
        authStatus.textContent = message;
    }

    function renderOutput(payload) {
        // API 返回值一律作为文本输出，防止邮件 HTML 或错误消息被浏览器执行。
        output.textContent = JSON.stringify(payload, null, 2);
    }

    function clearCredentials() {
        state.address = '';
        state.accessToken = '';
        keyInput.value = '';
        setAuthenticated(false);
    }

    async function parseResponse(response) {
        const raw = await response.text();
        if (!raw) {
            return null;
        }

        try {
            return JSON.parse(raw);
        } catch (_error) {
            return raw;
        }
    }

    function responsePayload(method, path, response, body) {
        return {
            requested_at: new Date().toISOString(),
            request: { method, path },
            response: {
                status: response.status,
                ok: response.ok,
                body
            }
        };
    }

    async function authenticate(event) {
        event.preventDefault();

        const address = addressInput.value.trim();
        const mailboxKey = keyInput.value.trim();
        if (!address || !mailboxKey) {
            setStatus('error', '请输入邮箱地址和邮箱密钥。');
            (!address ? addressInput : keyInput).focus();
            return;
        }

        clearCredentials();
        authButton.disabled = true;
        authButton.textContent = '鉴权中...';
        setStatus('pending', '正在换取访问令牌...');

        const path = '/api/get_mailbox_token';
        try {
            const response = await fetch(path, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({
                    address,
                    mailbox_key: mailboxKey
                })
            });
            const body = await parseResponse(response);

            if (!response.ok || !body || !body.access_token) {
                renderOutput(responsePayload('POST', path, response, body));
                setStatus('error', body?.message || body?.error || `鉴权失败（HTTP ${response.status}）`);
                return;
            }

            state.address = body.address || address;
            state.accessToken = body.access_token;
            keyInput.value = '';
            setAuthenticated(true);
            setStatus('success', `已鉴权：${state.address}`);

            // 不把 access_token 回显到页面，避免截图或复制输出时泄露凭据。
            renderOutput({
                requested_at: new Date().toISOString(),
                request: { method: 'POST', path },
                response: {
                    status: response.status,
                    ok: true,
                    body: {
                        success: true,
                        address: state.address,
                        mailbox_id: body.mailbox_id,
                        expires_at: body.expires_at,
                        message: body.message
                    }
                }
            });
        } catch (error) {
            renderOutput({
                requested_at: new Date().toISOString(),
                request: { method: 'POST', path },
                error: error.message
            });
            setStatus('error', '网络请求失败，请检查服务状态。');
        } finally {
            authButton.disabled = false;
            authButton.textContent = '获取访问令牌';
        }
    }

    async function runReadRequest(button, path) {
        if (!state.accessToken || !state.address) {
            setStatus('error', '请先完成邮箱鉴权。');
            return;
        }

        const originalLabel = button.textContent;
        button.disabled = true;
        button.textContent = '请求中...';

        try {
            const response = await fetch(path, {
                headers: {
                    'Authorization': `Bearer ${state.accessToken}`
                }
            });
            const body = await parseResponse(response);
            renderOutput(responsePayload('GET', path, response, body));

            if (response.ok) {
                setStatus('success', `请求成功：HTTP ${response.status}`);
            } else {
                setStatus('error', body?.message || body?.error || `请求失败（HTTP ${response.status}）`);
            }
        } catch (error) {
            renderOutput({
                requested_at: new Date().toISOString(),
                request: { method: 'GET', path },
                error: error.message
            });
            setStatus('error', '网络请求失败，请检查服务状态。');
        } finally {
            button.textContent = originalLabel;
            button.disabled = !state.accessToken;
        }
    }

    authForm.addEventListener('submit', authenticate);
    mailboxInfoButton.addEventListener('click', () => {
        runReadRequest(mailboxInfoButton, '/api/mailbox_info_v2');
    });
    inboxButton.addEventListener('click', () => {
        runReadRequest(inboxButton, `/api/get_inbox?address=${encodeURIComponent(state.address)}`);
    });
    clearSessionButton.addEventListener('click', () => {
        clearCredentials();
        authForm.reset();
        output.textContent = '等待请求...';
        setStatus('idle', '会话已清空。');
        addressInput.focus();
    });
    clearOutputButton.addEventListener('click', () => {
        output.textContent = '等待请求...';
        output.focus();
    });
});

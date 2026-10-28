'use strict';

document.addEventListener('DOMContentLoaded', () => {
    const form = document.getElementById('login-form');
    const addressInput = document.getElementById('mailbox-address');
    const keyInput = document.getElementById('mailbox-key');
    const passwordToggle = document.getElementById('password-toggle');

    // 触屏设备不抢占焦点，避免首次打开页面就被软键盘遮挡主操作。
    if (window.matchMedia('(hover: hover) and (pointer: fine)').matches) {
        addressInput.focus({ preventScroll: true });
    }

    form.addEventListener('submit', handleSubmit);
    addressInput.addEventListener('input', () => clearFieldError(addressInput, 'address-error'));
    keyInput.addEventListener('input', () => clearFieldError(keyInput, 'key-error'));
    passwordToggle.addEventListener('click', () => toggleSecretVisibility(
        keyInput, passwordToggle, document.getElementById('password-icon'), '密钥'
    ));

    // 管理员入口只做弹窗开关与状态重置，不影响公开邮箱登录流程。
    const adminEntry = document.getElementById('admin-entry');
    const adminDialog = document.getElementById('admin-login-dialog');
    const adminForm = document.getElementById('admin-login-form');
    const adminPasswordInput = document.getElementById('admin-login-password');
    const adminPasswordToggle = document.getElementById('admin-password-toggle');
    adminEntry.addEventListener('click', () => openAdminLoginDialog(adminDialog, adminEntry));
    adminForm.addEventListener('submit', handleAdminSubmit);
    adminPasswordInput.addEventListener('input', () => {
        document.getElementById('admin-login-error').textContent = '';
    });
    adminPasswordToggle.addEventListener('click', () => toggleSecretVisibility(
        adminPasswordInput, adminPasswordToggle, document.getElementById('admin-password-icon'), '密码'
    ));

    document.addEventListener('keydown', (event) => {
        if ((event.ctrlKey || event.metaKey) && event.key === 'Enter') {
            event.preventDefault();
            // 弹窗打开时快捷键作用于弹窗表单，避免误提交背后的邮箱登录。
            (adminDialog.open ? adminForm : form).requestSubmit();
        }
    });
});

function handleSubmit(event) {
    event.preventDefault();

    const addressInput = document.getElementById('mailbox-address');
    const keyInput = document.getElementById('mailbox-key');
    const address = addressInput.value.trim();
    const mailboxKey = keyInput.value.trim();
    let valid = true;

    if (!isValidEmail(address)) {
        setFieldError(addressInput, 'address-error', address ? '请输入有效的邮箱地址。' : '请输入邮箱地址。');
        valid = false;
    }

    if (!mailboxKey) {
        setFieldError(keyInput, 'key-error', '请输入邮箱密钥。');
        valid = false;
    } else if (!isValidMailboxKey(mailboxKey)) {
        setFieldError(keyInput, 'key-error', '仅支持字母、数字及 . _ ~ -，长度 6–128。');
        valid = false;
    }

    if (!valid) {
        showToast('请检查标记的输入项。', 'error');
        return;
    }

    // 密钥不做额外持久化；目标页从固定凭据地址重新认证。
    const credential = `${encodeURIComponent(address)}----${encodeURIComponent(mailboxKey)}`;
    window.location.assign(`/web/${credential}`);
}

function openAdminLoginDialog(dialog, opener) {
    const form = document.getElementById('admin-login-form');
    const passwordInput = document.getElementById('admin-login-password');
    const toggle = document.getElementById('admin-password-toggle');
    form.reset();
    // 重置后统一回到隐藏密码状态，避免上次会话的明文状态残留。
    passwordInput.type = 'password';
    toggle.setAttribute('aria-pressed', 'false');
    toggle.setAttribute('aria-label', '显示密码');
    const sprite = document.documentElement.dataset.iconSprite;
    document.getElementById('admin-password-icon').setAttribute('href', `${sprite}#eye`);
    document.getElementById('admin-login-error').textContent = '';
    MaildropUI.openDialog(dialog, opener);
}

async function handleAdminSubmit(event) {
    event.preventDefault();

    const passwordInput = document.getElementById('admin-login-password');
    const errorArea = document.getElementById('admin-login-error');
    const submitButton = document.getElementById('admin-login-submit');
    const password = passwordInput.value;
    errorArea.textContent = '';

    if (!password) {
        errorArea.textContent = '请输入管理员密码。';
        passwordInput.focus({ preventScroll: true });
        return;
    }

    MaildropUI.setBusy(submitButton, true, '验证中…');
    try {
        const response = await fetch('/api/admin_login', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ password })
        });
        const data = await response.json().catch(() => null);

        if (response.ok && data && data.success) {
            // 管理凭据只保留在当前标签会话；后台页面用同一份凭据继续认证。
            sessionStorage.setItem('admin_token', password);
            window.location.assign('/admin');
            return;
        }

        errorArea.textContent = adminLoginErrorMessage(response.status, data);
        passwordInput.focus({ preventScroll: true });
    } catch (error) {
        errorArea.textContent = '网络异常，请稍后重试。';
    } finally {
        MaildropUI.setBusy(submitButton, false);
    }
}

// 管理登录的失败语义由状态码决定；仅 IP 临时封禁时展示服务端给出的剩余时间提示。
function adminLoginErrorMessage(status, data) {
    if (status === 401) {
        return '密码错误';
    }
    if (status === 403) {
        const message = data && typeof data.message === 'string' ? data.message : '';
        return message.includes('封禁') ? message : '当前 IP 不允许管理员登录';
    }
    if (status === 503) {
        return '管理员密码未配置';
    }
    return '登录失败，请稍后重试';
}

function isValidEmail(value) {
    return /^[^\s@]+@[^\s@]+(?:\.[^\s@]+)*$/.test(value);
}

function isValidMailboxKey(value) {
    return /^[A-Za-z0-9._~-]{6,128}$/.test(value);
}

function setFieldError(input, errorId, message) {
    input.setAttribute('aria-invalid', 'true');
    input.closest('.input-shell').classList.add('is-invalid');
    document.getElementById(errorId).textContent = message;
}

function clearFieldError(input, errorId) {
    input.removeAttribute('aria-invalid');
    input.closest('.input-shell').classList.remove('is-invalid');
    document.getElementById(errorId).textContent = '';
}

function toggleSecretVisibility(input, button, icon, noun) {
    const show = input.type === 'password';

    input.type = show ? 'text' : 'password';
    button.setAttribute('aria-pressed', String(show));
    button.setAttribute('aria-label', show ? `隐藏${noun}` : `显示${noun}`);
    const sprite = document.documentElement.dataset.iconSprite;
    icon.setAttribute('href', `${sprite}#${show ? 'eye-off' : 'eye'}`);
    input.focus({ preventScroll: true });
}

function showToast(message, type = 'info') {
    MaildropUI.toast(message, type);
}

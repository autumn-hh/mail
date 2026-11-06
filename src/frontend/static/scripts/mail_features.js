'use strict';

const mailFeatures = { policy: null, replyId: '', draftKey: '', draftFingerprint: '', sending: false, sentOffset: 0, sentId: null, loadingSent: false, content: null };
const deliveryLabels = { queued: '排队中', retry: '等待重试', sending: '投递中', accepted: '对方服务器已接受', failed: '发送失败', partial_failed: '部分发送失败', bounced: '收到失败退信' };

document.addEventListener('DOMContentLoaded', () => {
    document.getElementById('compose-form').addEventListener('submit', submitMail);
    document.getElementById('compose-files').addEventListener('change', () => {
        const files = Array.from(document.getElementById('compose-files').files);
        document.getElementById('compose-file-summary').textContent = `${files.length} 个附件 · ${formatBytes(files.reduce((n, file) => n + file.size, 0))}`;
    });
    document.getElementById('clear-draft').addEventListener('click', clearDraft);
    document.getElementById('reply-current').addEventListener('click', replyToCurrent);
    document.getElementById('download-raw').addEventListener('click', () => {
        if (state.currentEmailId) downloadAuthenticated(`/api/emails/${encodeURIComponent(state.currentEmailId)}/raw`, 'message.eml').catch((error) => handleRequestError(error, '原文下载失败。'));
    });
    document.getElementById('refresh-sent').addEventListener('click', () => loadSent().catch((error) => handleRequestError(error, '发送记录加载失败。')));
    document.getElementById('sent-previous').addEventListener('click', () => { mailFeatures.sentOffset = Math.max(0, mailFeatures.sentOffset - 50); loadSent().catch((error) => handleRequestError(error, '加载失败。')); });
    document.getElementById('sent-next').addEventListener('click', () => { mailFeatures.sentOffset += 50; loadSent().catch((error) => handleRequestError(error, '加载失败。')); });
});

function formatBytes(bytes) { return `${(Number(bytes || 0) / 1048576).toFixed(2)} MiB`; }

async function loadSendingPolicy() {
    mailFeatures.policy = await apiFetch('/api/sending-policy');
    renderSendingPolicy();
}
function renderSendingPolicy() {
    const policy = mailFeatures.policy;
    if (!policy) return;
    const allowed = policy.enabled && policy.available;
    document.getElementById('compose-nav').hidden = !policy.enabled;
    document.getElementById('reply-current').hidden = !policy.enabled;
    document.getElementById('reply-current').disabled = !allowed;
    document.getElementById('compose-fields').disabled = !allowed || mailFeatures.sending;
    document.getElementById('compose-from').value = state.address;
    document.getElementById('sending-note').textContent = policy.enabled ? (policy.available ? '已获发件授权' : '发件暂不可用') : '发件需主管理员授权';
    document.getElementById('sending-quota').textContent = !policy.enabled ? '发件权限未开通或已撤销，请联系主管理员。' : !policy.available ? policy.reason : `今日 ${policy.daily_used}/${policy.daily_limit} 人（北京时间 00:00 重置） · 最近一分钟 ${policy.minute_used}/${policy.minute_limit} 人`;
    document.getElementById('sent-storage').textContent = `发件存档 ${formatBytes(policy.storage_used)} / ${formatBytes(policy.storage_limit)}，独立于收件容量。`;
}

async function fetchMailBlob(path) {
    if (!state.accessToken) throw new ApiError('请重新验证邮箱。', 401);
    const response = await fetch(path, { credentials: 'same-origin', headers: { Authorization: `Bearer ${state.accessToken}` } });
    if (!response.ok) throw new ApiError(getErrorMessage(await readJson(response), '下载失败。'), response.status);
    return response.blob();
}
async function downloadAuthenticated(path, filename) {
    const blob = await fetchMailBlob(path);
    const url = URL.createObjectURL(blob);
    const link = document.createElement('a');
    link.href = url; link.download = filename; link.rel = 'noreferrer';
    document.body.appendChild(link); link.click(); link.remove();
    window.setTimeout(() => URL.revokeObjectURL(url), 30_000);
}
function blobDataURL(blob) {
    return new Promise((resolve, reject) => { const reader = new FileReader(); reader.onload = () => resolve(reader.result); reader.onerror = () => reject(new Error('无法读取附件。')); reader.readAsDataURL(blob); });
}
function attachmentButton(path, name, size) {
    const button = document.createElement('button');
    button.className = 'button'; button.type = 'button'; button.textContent = `${name} · ${formatBytes(size)}`;
    button.addEventListener('click', async () => {
        button.disabled = true;
        try { await downloadAuthenticated(path, name); } catch (error) { handleRequestError(error, '附件下载失败。'); } finally { button.disabled = false; }
    });
    return button;
}

async function loadMailContent(email) {
    mailFeatures.content = null;
    document.getElementById('detail-auth').replaceChildren();
    document.getElementById('detail-attachments').replaceChildren();
    document.getElementById('download-raw').disabled = true;
    let content;
    try { content = await apiFetch(`/api/emails/${encodeURIComponent(email.id)}/content`); }
    catch (error) { if (error.status === 404) return; throw error; }
    if (state.currentEmailId !== String(email.id)) return;
    mailFeatures.content = content;
    document.getElementById('download-raw').disabled = false;
    const auth = content.authentication;
    const authContainer = document.getElementById('detail-auth');
    if (auth) {
        const checks = [['SPF', auth.spf], ...((auth.dkim || []).map((result) => ['DKIM', result])), ['DMARC', auth.dmarc]];
        const names = { pass: '通过', fail: '未通过', softfail: '软失败', neutral: '中立', none: '无记录', temperror: '查询暂时异常', permerror: '记录无效' };
        checks.forEach(([label, result]) => {
            if (!result) return;
            const badge = document.createElement('span'); badge.className = 'auth-badge';
            if (['fail', 'softfail', 'permerror', 'temperror'].includes(result.result)) badge.classList.add('is-fail');
            badge.textContent = `${label}：${names[result.result] || result.result}${result.domain ? ` · ${result.domain}` : ''}`;
            badge.title = result.reason || ''; authContainer.appendChild(badge);
        });
    } else { authContainer.textContent = '未执行身份验证。'; }
    (content.warnings || []).forEach((warning) => { const note = document.createElement('span'); note.textContent = warning; authContainer.appendChild(note); });
    if (content.to?.length) document.getElementById('detail-to').textContent = content.to.join(', ');
    if (content.cc?.length) { const note = document.createElement('span'); note.textContent = `抄送：${content.cc.join(', ')}`; authContainer.appendChild(note); }
    const attachments = content.attachments || [];
    const list = document.getElementById('detail-attachments');
    attachments.forEach((a) => list.appendChild(attachmentButton(`/api/emails/${encodeURIComponent(email.id)}/attachments/${encodeURIComponent(a.id)}`, a.filename, a.size)));
    const images = new Map();
    let total = 0;
    // 内嵌图必须由当前邮箱授权下载；只展示常见位图，限制自动解码体积。
    for (const attachment of attachments) {
        if (!attachment.content_id || !/^image\/(png|jpeg|gif|webp)$/i.test(attachment.content_type) || total + attachment.size > 10 * 1048576) continue;
        total += attachment.size;
        if (state.currentEmailId !== String(email.id)) return;
        const blob = await fetchMailBlob(`/api/emails/${encodeURIComponent(email.id)}/attachments/${encodeURIComponent(attachment.id)}`);
        images.set(attachment.content_id, await blobDataURL(blob));
    }
    if (state.currentEmailId === String(email.id) && images.size) renderMessageBody({ ...email, inlineImages: images });
}

function splitRecipients(value) { return value.split(/[,;\n]+/).map((part) => part.trim()).filter(Boolean); }
function mailRequestKey() {
    // getRandomValues 也适用于普通 HTTP；随机标识在响应丢失后沿用。
    const bytes = crypto.getRandomValues(new Uint8Array(16));
    return Array.from(bytes, (value) => value.toString(16).padStart(2, '0')).join('');
}
function clearDraft() {
    if (mailFeatures.sending) return;
    document.getElementById('compose-form').reset();
    document.getElementById('compose-from').value = state.address;
    document.getElementById('compose-result').textContent = '';
    document.getElementById('compose-file-summary').textContent = '';
    document.getElementById('compose-reply-note').hidden = true;
    mailFeatures.replyId = ''; mailFeatures.draftKey = ''; mailFeatures.draftFingerprint = '';
}
function replyToCurrent() {
    if (!mailFeatures.policy?.enabled) return;
    const email = state.emails.find((item) => String(item.id) === state.currentEmailId);
    if (!email) return;
    const recipients = mailFeatures.content?.reply_to?.length ? mailFeatures.content.reply_to : [email.From];
    clearDraft();
    document.getElementById('compose-to').value = recipients.join(', ');
    document.getElementById('compose-subject').value = /^re:/i.test(email.Subject || '') ? email.Subject : `Re: ${email.Subject || ''}`;
    const plain = mailFeatures.content?.text || (String(email.ContentType).toLowerCase().includes('html') ? stripHtml(email.Body || '') : email.Body || '');
    document.getElementById('compose-text').value = `\n\n${email.From} 写道：\n${plain.split(/\r?\n/).map((line) => `> ${line}`).join('\n')}`;
    mailFeatures.replyId = email.id;
    const note = document.getElementById('compose-reply-note'); note.hidden = false; note.textContent = '此邮件将关联原邮件的回复主题。';
    navigateToView('compose'); document.getElementById('compose-text').focus();
}
async function submitMail(event) {
    event.preventDefault();
    if (mailFeatures.sending) return;
    const result = document.getElementById('compose-result');
    mailFeatures.sending = true;
    document.getElementById('compose-fields').disabled = true;
    try {
        const files = Array.from(document.getElementById('compose-files').files);
        if (files.length > 10 || files.reduce((n, file) => n + file.size, 0) > 10 * 1048576) throw new Error('附件最多 10 个，合计不能超过 10 MiB。');
        const attachments = [];
        for (const file of files) attachments.push({ filename: file.name, content_type: file.type || 'application/octet-stream', content: (await blobDataURL(file)).split(',')[1] });
        const request = { to: splitRecipients(document.getElementById('compose-to').value), cc: splitRecipients(document.getElementById('compose-cc').value), bcc: splitRecipients(document.getElementById('compose-bcc').value), subject: document.getElementById('compose-subject').value, text: document.getElementById('compose-text').value, html: document.getElementById('compose-html').value, attachments, reply_email_id: mailFeatures.replyId };
        const encoded = JSON.stringify(request);
        // 网络失败后保留同一请求的标识，用户重试不会重复入队。
        if (mailFeatures.draftFingerprint !== encoded) { mailFeatures.draftKey = mailRequestKey(); mailFeatures.draftFingerprint = encoded; }
        result.textContent = '正在提交邮件…';
        const sent = await apiFetch('/api/send', { method: 'POST', headers: { 'Idempotency-Key': mailFeatures.draftKey }, body: encoded });
        result.textContent = `邮件已加入队列：${sent.id}`;
        mailFeatures.sending = false; clearDraft();
        showToast('邮件已加入投递队列。', 'success');
        mailFeatures.sentOffset = 0; mailFeatures.sentId = sent.id; navigateToView('sent');
        await loadSendingPolicy();
    } catch (error) {
        result.textContent = error.message || '提交失败，请重试。';
        // 撤销发件权限、旧幂等记录过期都不等于收件会话失效。
        if (error instanceof ApiError && [403, 410].includes(error.status)) {
            if (error.status === 403) {
                mailFeatures.policy = { ...mailFeatures.policy, enabled: false };
                renderSendingPolicy();
            }
            try { await loadSendingPolicy(); showToast(result.textContent, 'error'); }
            catch (authError) { handleRequestError(authError, '邮箱状态读取失败。'); }
        } else { handleRequestError(error, '邮件提交失败。'); }
    } finally { mailFeatures.sending = false; document.getElementById('compose-fields').disabled = !(mailFeatures.policy?.enabled && mailFeatures.policy?.available); }
}

async function loadSent() {
    if (mailFeatures.loadingSent) return;
    mailFeatures.loadingSent = true;
    try {
        const items = await apiFetch(`/api/sent?offset=${mailFeatures.sentOffset}`);
        const list = document.getElementById('sent-list'); list.replaceChildren();
        if (!items.length) { const message = document.createElement('p'); message.className = 'sent-detail-content'; message.textContent = '暂无发送记录。'; list.appendChild(message); }
        for (const item of items) {
            const button = document.createElement('button'); button.className = 'sent-row'; button.type = 'button';
            const title = document.createElement('strong'); title.textContent = item.details.subject || '（无主题）';
            const status = document.createElement('span'); status.textContent = deliveryLabels[item.status] || item.status;
            const note = document.createElement('small'); note.textContent = `${formatFullDate(item.created_at)} · ${item.recipients.length} 个收件人${item.bounce_count ? ` · ${item.bounce_count} 份退信报告` : ''}`;
            button.append(title, status, note); button.addEventListener('click', () => { mailFeatures.sentId = item.id; loadSentDetail().catch((error) => handleRequestError(error, '发送详情加载失败。')); }); list.appendChild(button);
        }
        document.getElementById('sent-previous').disabled = mailFeatures.sentOffset === 0;
        document.getElementById('sent-next').disabled = items.length < 50;
        if (mailFeatures.sentId) await loadSentDetail();
    } finally { mailFeatures.loadingSent = false; }
}
async function loadSentDetail() {
    const id = mailFeatures.sentId;
    const sent = await apiFetch(`/api/sent/${encodeURIComponent(id)}`);
    if (id !== mailFeatures.sentId) return;
    const container = document.getElementById('sent-detail'); container.hidden = false; container.replaceChildren();
    const content = document.createElement('div'); content.className = 'sent-detail-content';
    const title = document.createElement('h3'); title.textContent = sent.details.subject || '（无主题）'; content.appendChild(title);
    for (const [label, addresses] of [['收件人', sent.details.to], ['抄送', sent.details.cc], ['密送（仅自己可见）', sent.details.bcc]]) {
        if (addresses?.length) { const row = document.createElement('p'); row.textContent = `${label}：${addresses.join(', ')}`; content.appendChild(row); }
    }
    for (const recipient of sent.recipients) {
        const row = document.createElement('div'); row.className = 'sent-recipient';
        row.textContent = `${recipient.address} · ${deliveryLabels[recipient.status] || recipient.status} · 尝试 ${recipient.attempts} 次${recipient.uncertain ? ' · 曾出现确认不明，存在重复投递风险' : ''}${recipient.error ? ` · ${recipient.error}` : ''}`;
        content.appendChild(row);
    }
    const note = document.createElement('p'); note.className = 'view-subtitle'; note.textContent = '服务器接受不代表最终送达；退信为收到的报告，无法证明发件方身份。'; content.appendChild(note);
    const body = document.createElement('div'); body.className = 'message-body';
    if (sent.details.html) {
        const frame = document.createElement('iframe'); frame.className = 'message-frame'; frame.title = '发件正文'; frame.setAttribute('sandbox', ''); frame.setAttribute('referrerpolicy', 'no-referrer'); frame.srcdoc = buildIsolatedHtml(sent.details.html); body.appendChild(frame);
    } else { const pre = document.createElement('pre'); pre.className = 'message-text'; pre.textContent = sent.details.text || '正文为空。'; body.appendChild(pre); }
    content.appendChild(body);
    const actions = document.createElement('div'); actions.className = 'heading-actions';
    const raw = attachmentButton(`/api/sent/${encodeURIComponent(id)}/raw`, 'sent.eml', 0); raw.textContent = '下载原文 .eml'; actions.appendChild(raw);
    (sent.details.attachments || []).forEach((a) => actions.appendChild(attachmentButton(`/api/sent/${encodeURIComponent(id)}/attachments/${encodeURIComponent(a.id)}`, a.filename, a.size)));
    (sent.reports || []).forEach((report) => { const button = attachmentButton(`/api/delivery-reports/${encodeURIComponent(report.id)}/raw`, 'delivery-report.eml', 0); button.textContent = report.summary || '下载退信报告'; actions.appendChild(button); });
    if (!sent.recipients.some((r) => ['queued', 'retry', 'sending'].includes(r.status))) {
        const remove = document.createElement('button'); remove.type = 'button'; remove.className = 'button button-danger'; remove.textContent = '删除此发送记录';
        remove.addEventListener('click', async () => {
            remove.disabled = true;
            try { await apiFetch(`/api/sent/${encodeURIComponent(id)}`, { method: 'DELETE' }); mailFeatures.sentId = null; container.hidden = true; await Promise.all([loadSent(), loadSendingPolicy()]); }
            catch (error) { handleRequestError(error, '删除失败。'); remove.disabled = false; }
        }); actions.appendChild(remove);
    }
    content.appendChild(actions); container.appendChild(content);
}

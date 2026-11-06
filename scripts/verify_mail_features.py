"""收发改造的真实浏览器验收；全部数据和 DKIM 密钥在临时目录，收件域名使用 .invalid。"""
from __future__ import annotations

import json
import smtplib
from email.message import EmailMessage
from pathlib import Path
from urllib.request import Request, urlopen

from playwright.sync_api import sync_playwright
from verify_ui_browser import ADMIN_PASSWORD, ROOT, frontend_digest, isolated_site, mailbox_path, show_surface


def run():
    output = ROOT / "reports" / "ui-redesign" / "mail-features"
    output.mkdir(parents=True, exist_ok=True)
    source_sha256 = frontend_digest()
    checks = []
    errors = []
    with isolated_site(mail_features=True) as (base, fixtures, _):
        box = fixtures["boxes"]["studio"]

        def admin(method, path, data=None):
            request = Request(base + path, method=method, headers={"Authorization": "Bearer " + ADMIN_PASSWORD, "Content-Type": "application/json"}, data=None if data is None else json.dumps(data).encode())
            with urlopen(request, timeout=10) as response:
                return json.load(response)

        def received_ids():
            request = Request(base + "/api/get_inbox?address=" + box["address"], headers={"Authorization": "Bearer " + box["access_token"]})
            with urlopen(request, timeout=10) as response:
                return {mail["id"] for mail in json.load(response)}

        before = received_ids()
        message = EmailMessage()
        message["From"] = "Customer <customer@recipient.invalid>"
        message["Reply-To"] = "service@recipient.invalid"
        message["To"] = box["address"]
        message["Subject"] = "附件和回复验收"
        message["Message-ID"] = "<mail-feature-test@recipient.invalid>"
        message.set_content("客户正文")
        message.add_alternative('<p>客户正文<img src="cid:logo"></p><script>parent.attack=true</script>', subtype="html")
        # Minimal PNG is a real image, so naturalWidth also verifies cid replacement.
        import base64
        image = base64.b64decode("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
        message.get_payload()[1].add_related(image, maintype="image", subtype="png", cid="<logo>", filename="logo.png")
        message.add_attachment(b"attachment-fixture", maintype="application", subtype="octet-stream", filename="sample.bin")
        nested = EmailMessage(); nested["Subject"] = "转发的邮件"; nested.set_content("转发正文不能覆盖外层")
        message.add_attachment(nested, filename="forwarded.eml")
        with smtplib.SMTP("127.0.0.1", fixtures["smtp_port"], timeout=10) as client:
            client.send_message(message, from_addr="customer@recipient.invalid", to_addrs=[box["address"]])
        mail_id = (received_ids() - before).pop()

        with sync_playwright() as playwright:
            # 保留域名以普通 HTTP 打开，覆盖非安全上下文；请求转到实际隔离服务。
            browser = playwright.chromium.launch()
            browser_base = base.replace("127.0.0.1", "maildrop-ui.test")
            context = browser.new_context(viewport={"width": 1440, "height": 1050}, accept_downloads=True, reduced_motion="reduce")
            def isolated_route(route):
                response = route.fetch(url=route.request.url.replace(browser_base, base, 1))
                route.fulfill(response=response)
            context.route(browser_base + "/**", isolated_route)
            page = context.new_page(); page.set_default_timeout(15000)
            page.on("pageerror", lambda error: errors.append(str(error)))
            try:
                page.goto(browser_base + mailbox_path(box))
                page.locator("#mailbox-app").wait_for(state="visible")
                page.wait_for_function("() => mailFeatures.policy !== null")
                assert page.locator("#compose-nav").is_hidden()
                assert not page.evaluate("window.isSecureContext")
                assert page.evaluate("typeof crypto.randomUUID") == "undefined"
                checks.append("未授权邮箱隐藏发件入口")

                page.locator(f'.email-row[data-email-id="{mail_id}"] .email-open').click()
                page.wait_for_function("() => !document.getElementById('download-raw').disabled")
                assert page.locator("#detail-attachments button").count() == 3
                frame = page.frame_locator("#detail-body iframe")
                frame.locator('img[src^="data:image/png"]').wait_for()
                assert frame.locator("img").evaluate("image => image.complete && image.naturalWidth > 0")
                assert not page.evaluate("Boolean(window.attack)")
                assert "客户正文" in frame.locator("body").inner_text()
                assert "转发正文不能覆盖外层" not in frame.locator("body").inner_text()
                with page.expect_download() as pending:
                    page.get_by_role("button", name="sample.bin", exact=False).click()
                assert Path(pending.value.path()).read_bytes() == b"attachment-fixture"
                with page.expect_download() as pending:
                    page.locator("#download-raw").click()
                assert b"multipart/mixed" in Path(pending.value.path()).read_bytes()
                page.screenshot(path=str(output / "received-attachments.png"), full_page=True)
                checks.append("带鉴权附件和原文下载、CID 图像、转发附件及 HTML 隔离")

                admin_page = context.new_page()
                show_surface(admin_page, browser_base, fixtures, "admin-mailboxes")
                admin_page.evaluate("id => adminManager.editMailbox(id)", box["id"])
                admin_page.locator("#edit-sending-enabled").wait_for(state="visible")
                admin_page.locator("#edit-sending-enabled").check()
                admin_page.locator("#edit-daily-limit").fill("100")
                admin_page.locator("#edit-minute-limit").fill("10")
                admin_page.locator('[data-action="save-sending"]').click()
                admin_page.locator('[data-role="sending-result"]').filter(has_text="已保存").wait_for()
                admin_page.screenshot(path=str(output / "admin-sending-policy.png"), full_page=True)
                admin_page.close()
                checks.append("主管理员在实际页面授权并保存默认额度")

                page.evaluate("async () => await loadSendingPolicy()")
                page.locator("#reply-current").click()
                assert page.locator("#compose-to").input_value() == "service@recipient.invalid"
                assert page.locator("#compose-subject").input_value().startswith("Re:")
                page.locator("#compose-cc").fill("copy@recipient.invalid")
                page.locator("#compose-bcc").fill("private@recipient.invalid")
                page.locator("#compose-files").set_input_files({"name": "reply.txt", "mimeType": "text/plain", "buffer": b"browser outgoing attachment"})
                for width in (320, 390, 768, 900, 1440):
                    page.set_viewport_size({"width": width, "height": 1050})
                    assert page.evaluate("document.documentElement.scrollWidth <= innerWidth + 1")
                    assert page.locator(".view-nav").evaluate("nav => Array.from(nav.querySelectorAll('button:not([hidden])')).every(button => button.getBoundingClientRect().height <= 60)"), "导航文字不应竖排"
                    page.screenshot(path=str(output / f"compose-{width}.png"), full_page=True)
                # Lose first HTTP response AFTER the real server enqueued the request.
                def lose_response(route):
                    route.fetch(url=base + "/api/send")
                    route.abort("connectionfailed")
                page.route("**/api/send", lose_response, times=1)
                page.locator("#send-button").click()
                page.wait_for_function("() => !mailFeatures.sending && Boolean(mailFeatures.draftKey)")
                page.locator("#send-button").click()
                page.locator("#sent-view").wait_for(state="visible")
                page.locator("#sent-detail").wait_for(state="visible")
                assert page.locator("#sent-list .sent-row").count() == 1
                page.wait_for_function("() => mailFeatures.policy.daily_used === 3")
                assert "密送（仅自己可见）：private@recipient.invalid" in page.locator("#sent-detail").inner_text()
                with page.expect_download() as pending:
                    page.locator("#sent-detail").get_by_role("button", name="下载原文 .eml", exact=True).click()
                sent_raw = Path(pending.value.path()).read_bytes()
                assert b"Bcc:" not in sent_raw and b"private@recipient.invalid" not in sent_raw
                assert b"DKIM-Signature:" in sent_raw
                assert b"In-Reply-To: <mail-feature-test@recipient.invalid>" in sent_raw
                with page.expect_download() as pending:
                    page.locator("#sent-detail").get_by_role("button", name="reply.txt", exact=False).click()
                assert Path(pending.value.path()).read_bytes() == b"browser outgoing attachment"
                page.screenshot(path=str(output / "sent-record.png"), full_page=True)
                checks.append("普通 HTTP 下回复、To/Cc/Bcc 计额、附件、DKIM、网络失败重试幂等与 Bcc 保密")

                page.locator("#compose-nav").click()
                page.locator("#compose-to").fill("after-revoke@recipient.invalid")
                page.locator("#compose-subject").fill("撤权窗口内的提交")
                admin("PUT", f"/api/admin/mailboxes/{box['id']}/sending-policy", {"enabled": False, "daily_limit": 100, "minute_limit": 10})
                # 页面仍持有授权时的旧策略，直接提交必须被后端拒绝并同步权限。
                with page.expect_response("**/api/send") as denied:
                    page.locator("#send-button").click()
                assert denied.value.status == 403
                page.wait_for_function("() => !mailFeatures.sending && mailFeatures.policy.enabled === false")
                assert page.locator("#compose-nav").is_hidden()
                assert page.locator("#compose-fields").evaluate("fields => fields.disabled")
                assert page.locator("#send-button").is_disabled()
                assert page.locator("#mailbox-app").is_visible()
                page.locator('.nav-button[data-view="inbox"]').click()
                with page.expect_response("**/api/get_inbox?*") as refreshed:
                    page.locator("#refresh-button").click()
                assert refreshed.value.status == 200
                assert page.locator(".email-row").count() > 0
                assert page.evaluate("Boolean(state.accessToken && sessionStorage.getItem(SESSION_TOKEN_KEY) === state.accessToken)")
                assert page.locator("#mailbox-app").is_visible()
                checks.append("撤权窗口内提交被拒、表单关闭且仍可访问收件箱")

                # 重新授权后再次撤权；即使后续策略查询断网，也立即关闭旧的发件表单。
                admin("PUT", f"/api/admin/mailboxes/{box['id']}/sending-policy", {"enabled": True, "daily_limit": 100, "minute_limit": 10})
                page.evaluate("async () => await loadSendingPolicy()")
                with page.expect_response("**/api/sending-policy"):
                    page.locator("#compose-nav").click()
                page.locator("#compose-subject").fill("撤权后策略刷新断网")
                admin("PUT", f"/api/admin/mailboxes/{box['id']}/sending-policy", {"enabled": False, "daily_limit": 100, "minute_limit": 10})
                page.route("**/api/sending-policy", lambda route: route.abort("connectionfailed"), times=1)
                with page.expect_response("**/api/send") as denied:
                    page.locator("#send-button").click()
                assert denied.value.status == 403
                page.wait_for_function("() => !mailFeatures.sending && mailFeatures.policy.enabled === false")
                assert page.locator("#compose-nav").is_hidden()
                assert page.locator("#compose-fields").evaluate("fields => fields.disabled")
                assert page.locator("#mailbox-app").is_visible()
                page.locator('.nav-button[data-view="inbox"]').click()
                with page.expect_response("**/api/get_inbox?*") as refreshed:
                    page.locator("#refresh-button").click()
                assert refreshed.value.status == 200
                checks.append("撤权后策略刷新断网仍关闭发件入口并保留收件会话")
                assert not errors, errors
                assert source_sha256 == frontend_digest(), "前端源码在验收期间改变"
            except Exception as error:
                page.screenshot(path=str(output / "failure.png"), full_page=True)
                (output / "report.json").write_text(json.dumps({"passed": False, "checks": checks, "error": str(error), "browser_errors": errors, "source_sha256": source_sha256}, ensure_ascii=False, indent=2), encoding="utf-8")
                raise
            finally:
                context.close(); browser.close()
    report = {"passed": True, "checks": checks, "browser_errors": errors, "isolated_server_stopped": True, "source_sha256": source_sha256}
    (output / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
    print(json.dumps(report, ensure_ascii=False))

if __name__ == "__main__":
    run()

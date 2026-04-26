package i18n

// bundleRU is the Russian translation map. Every key in bundleEN must have
// a counterpart here — the TestBundle_RUKeysMatchEN sanity test in
// i18n_test.go enforces parity. Printf placeholders (%s) are preserved
// verbatim so callers can fmt.Sprintf the same arg list against either
// language.
var bundleRU = map[string]string{
	// Buttons (web).
	"button.copy":            "Копировать",
	"button.create.share":    "Создать ссылку",
	"button.download":        "Скачать",
	"button.download.astext": "Скачать как .txt",
	"button.logout":          "Выйти",
	"button.unlock":          "Открыть",

	// Bot replies. Plurals on "час/часа/часов" etc. are handled at the bundle
	// layer for the small fixed set we ship; full ICU plural forms are a
	// Phase 14 concern.
	"bot.reply.error.filetoolarge":             "Файл слишком большой (максимум %s).",
	"bot.reply.error.filetoolarge_predownload": "Файл слишком большой (максимум %s).",
	"bot.reply.error.generic":                  "Что-то пошло не так. Попробуйте ещё раз через минуту.",
	"bot.reply.help": "Пришлите мне файл или текстовое сообщение — я сохраню его и отвечу короткой ссылкой.\n\n" +
		"Ссылки действуют %s. Пользоваться ботом могут только аккаунты Telegram из белого списка.\n\n" +
		"Команды администратора (/allow, /revoke, /users) появятся в одной из следующих фаз.",
	"bot.reply.share.file": "✓ Сохранён %s (%s). Ссылка: %s\nИстекает: %s",
	"bot.reply.share.text": "✓ Сохранено как текст. Ссылка: %s\nИстекает: %s",
	"bot.reply.start": "Пришлите мне файл или текстовое сообщение — я сохраню его и отвечу короткой ссылкой.\n\n" +
		"Ссылки действуют %s. Пользоваться ботом могут только аккаунты Telegram из белого списка.",
	"bot.reply.weblogin.link":        "Ссылка для входа (действует 5 минут):\n%s",
	"bot.reply.weblogin.nonprivate":  "Отправьте /weblogin мне в личные сообщения — ссылку для входа нельзя публиковать в групповых чатах.",
	"bot.reply.weblogin.ratelimited": "Вы недавно уже запрашивали ссылку для входа — посмотрите ранее присланные сообщения. Попробуйте снова через минуту.",

	// Auth errors surfaced through the /login?error=… banner.
	"error.auth.access_denied":     "Доступ запрещён — ваш аккаунт Telegram не имеет прав на вход.",
	"error.auth.invalid_link":      "Эта ссылка для входа недействительна.",
	"error.auth.invalid_signature": "Подпись Telegram не прошла проверку. Попробуйте ещё раз.",
	"error.auth.link_expired":      "Срок действия ссылки истёк. Отправьте /weblogin боту, чтобы получить новую.",
	"error.auth.link_used":         "Эта ссылка для входа уже была использована.",

	// Generic error template (status-code → title + body).
	"error.badrequest.form_read":          "Не удалось прочитать отправленную форму.",
	"error.badrequest.share_notprotected": "Эта ссылка не защищена паролем.",
	"error.badrequest.title":              "Некорректный запрос",
	"error.gone.share_expired":            "Срок действия этой ссылки истёк.",
	"error.gone.title":                    "Удалено",
	"error.internal.message":              "Произошла внутренняя ошибка.",
	"error.internal.title":                "Что-то пошло не так",
	"error.notfound.message":              "Такой ссылки не существует.",
	"error.notfound.title":                "Не найдено",
	"error.password.incorrect":            "Неверный пароль",
	"error.share.unavailable":             "Не удалось отобразить эту ссылку.",
	"error.storage.missing":               "Содержимое для этой ссылки недоступно.",
	"error.upload.failed":                 "Не удалось создать ссылку. Попробуйте ещё раз.",
	"error.upload.parse":                  "Не удалось обработать форму. Проверьте поля и попробуйте ещё раз.",
	"error.upload.toolarge":               "Загружаемый файл слишком большой — лимит %s.",
	"error.upload.toolargetext":           "Текст слишком длинный — лимит %s.",

	// Password unlock form.
	"form.password.label": "Пароль",

	// Upload form.
	"form.upload.expiry.1h":      "1 час",
	"form.upload.expiry.6h":      "6 часов",
	"form.upload.expiry.24h":     "24 часа",
	"form.upload.expiry.3d":      "3 дня",
	"form.upload.expiry.7d":      "7 дней",
	"form.upload.expiry.30d":     "30 дней",
	"form.upload.help.filesize":  "Максимальный размер файла: %s.",
	"form.upload.help.textsize":  "Максимальный размер текста: %s.",
	"form.upload.label.expiry":   "Срок действия",
	"form.upload.label.file":     "Файл",
	"form.upload.label.password": "Пароль (необязательно)",
	"form.upload.label.text":     "Текст",
	"form.upload.legend.kind":    "Тип ссылки",
	"form.upload.option.file":    "Файл",
	"form.upload.option.text":    "Текст",

	// JS feedback strings injected via data-* attributes on the copy button.
	"js.copy.failed":  "Не удалось скопировать",
	"js.copy.success": "Скопировано!",

	// Page chrome. Login fallback body matches the SPEC's drafted RU text
	// verbatim, segmented to leave the template room to render the bot
	// @username link and the /weblogin token between the pieces.
	"page.default.title":                  "yacht",
	"page.home.create_share.description":  "— загрузите файл или вставьте текст, чтобы получить короткую ссылку.",
	"page.home.create_share.link":         "Создать ссылку",
	"page.home.heading":                   "Вы вошли",
	"page.home.title":                     "yacht",
	"page.home.welcome.anonymous":         "Добро пожаловать.",
	"page.home.welcome.named":             "Добро пожаловать, %s.",
	"page.login.description":              "Войдите через свой аккаунт Telegram.",
	"page.login.fallback.body.before":     "Возможно, виджет заблокирован вашей сетью. Откройте",
	"page.login.fallback.body.middle":     "в Telegram и отправьте",
	"page.login.fallback.body.outro":      "— бот пришлёт одноразовую ссылку для входа.",
	"page.login.fallback.heading":         "Не видите кнопку «Войти через Telegram»?",
	"page.login.heading":                  "Вход",
	"page.login.title":                    "Вход — yacht",
	"page.password.heading":               "Требуется пароль",
	"page.password.title":                 "Требуется пароль — yacht",
	"page.share_created.create_another":   "Создать ещё одну ссылку",
	"page.share_created.description":      "Ссылка готова. Скопируйте её ниже, чтобы поделиться.",
	"page.share_created.expires_at":       "Истекает %s",
	"page.share_created.heading":          "Ссылка создана",
	"page.share_created.title":            "Ссылка создана — yacht",
	"page.share_created.view_text":        "Открыть текстовую ссылку",
	"page.share_file.meta":                "%s · истекает %s",
	"page.share_file.title":               "%s — yacht",
	"page.share_text.heading":             "Текстовая ссылка",
	"page.share_text.meta":                "истекает %s",
	"page.share_text.title":               "Текстовая ссылка — yacht",
	"page.upload.heading":                 "Создать ссылку",
	"page.upload.title":                   "Загрузка — yacht",
}

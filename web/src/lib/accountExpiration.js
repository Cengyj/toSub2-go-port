function expirationDate(value) {
  if (typeof value !== "string") return null;
  // Persisted expiry is RFC3339. Date's string parser also accepts local times
  // and can normalize nonexistent calendar dates, hiding invalid metadata.
  const parts = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d+))?(Z|([+-])(\d{2}):(\d{2}))$/.exec(value);
  if (!parts || parts[0] !== value) return null;
  const [, yearText, monthText, dayText, hourText, minuteText, secondText, fraction = ""] = parts;
  const [year, month, day, hour, minute, second] = [yearText, monthText, dayText, hourText, minuteText, secondText].map(Number);
  const leapYear = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const daysInMonth = [31, leapYear ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  const offsetHours = Number(parts[10] || 0), offsetMinutes = Number(parts[11] || 0);
  if (year < 1 || month < 1 || month > 12 || day < 1 || day > daysInMonth[month - 1] || hour > 23 || minute > 59 || second > 59 || offsetHours > 23 || offsetMinutes > 59) return null;
  const date = new Date(0);
  // setUTCFullYear preserves years 0001–0099; Date.UTC would add 1900.
  date.setUTCFullYear(year, month - 1, day);
  date.setUTCHours(hour, minute, second, Number(fraction.slice(0, 3).padEnd(3, "0")));
  const offset = (offsetHours * 60 + offsetMinutes) * (parts[9] === "-" ? -1 : 1);
  date.setTime(date.getTime() - offset * 60000);
  if (date.getUTCFullYear() < 1 || date.getUTCFullYear() > 9999) return null;
  // Match Go's zero-time sentinel without rejecting a nonzero nanosecond
  // fraction merely because browser Date displays millisecond precision.
  if (date.toISOString() === "0001-01-01T00:00:00.000Z" && !/[1-9]/.test(fraction.slice(0, 9))) return null;
  return date;
}

export function browserTimeZone() {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "";
  } catch {
    return "";
  }
}

export function expirationInputValue(value) {
  const date = expirationDate(value);
  if (!date) return "";
  return formatExpirationInput(date);
}

function formatExpirationInput(date) {
  const pad = (part) => String(part).padStart(2, "0");
  return `${String(date.getFullYear()).padStart(4, "0")}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

export function describeExpiration(job = {}, now = Date.now()) {
  // Only account/subscription metadata is eligible. OAuth token lifetimes are
  // intentionally not consulted, even if no account expiration is available.
  let date = expirationDate(job.manualExpiresAt);
  let source = date ? "manual" : "unknown";
  if (!date) {
    date = expirationDate(job.subscriptionExpiresAt);
    if (date) source = "subscription";
  }
  if (!date && ["manual", "subscription"].includes(job.expiresAtSource)) {
    date = expirationDate(job.expiresAt);
    if (date) source = job.expiresAtSource;
  }
  if (!date) return {source: "unknown", sourceLabel: "未知", dateTime: "", label: "未提供到期时间", remaining: "", expired: false, tone: "neutral"};
  const remainingMs = date.getTime() - now;
  const expired = remainingMs <= 0;
  const minutes = Math.floor(remainingMs / 60000);
  const days = Math.floor(minutes / 1440);
  const hours = Math.floor(minutes / 60);
  const remaining = expired ? "已到期" : days ? `剩余 ${days} 天` : hours ? `剩余 ${hours} 小时${minutes % 60 ? ` ${minutes % 60} 分钟` : ""}` : minutes ? `剩余 ${minutes} 分钟` : "不足 1 分钟";
  return {source, sourceLabel: source === "manual" ? "手动" : "套餐", dateTime: date.toISOString(), label: formatExpirationInput(date).replace("T", " "), remaining, expired, tone: expired ? "expired" : remainingMs <= 86400000 ? "soon" : "neutral"};
}

/**
 * Theme state. Dark is the default; the choice is persisted to localStorage and
 * applied to <html data-theme>. Initial resolution order (matches the pre-paint
 * script in index.html): stored choice → prefers-color-scheme → dark.
 */
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";

export type Theme = "dark" | "light";

const STORAGE_KEY = "aac-theme";

function readStored(): Theme | null {
  try {
    const v = localStorage.getItem(STORAGE_KEY);
    return v === "light" || v === "dark" ? v : null;
  } catch {
    return null;
  }
}

function systemTheme(): Theme {
  return typeof window !== "undefined" &&
    window.matchMedia("(prefers-color-scheme: light)").matches
    ? "light"
    : "dark";
}

function apply(theme: Theme) {
  document.documentElement.setAttribute("data-theme", theme);
}

interface ThemeContextValue {
  theme: Theme;
  /** Whether the user has explicitly chosen (vs following the system). */
  isExplicit: boolean;
  setTheme: (t: Theme) => void;
  toggleTheme: () => void;
}

const ThemeContext = createContext<ThemeContextValue | null>(null);

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [stored, setStored] = useState<Theme | null>(() => readStored());
  const [system, setSystem] = useState<Theme>(() => systemTheme());

  const theme = stored ?? system;

  // Keep <html> in sync whenever the resolved theme changes.
  useEffect(() => {
    apply(theme);
  }, [theme]);

  // Follow the OS setting only while the user hasn't made an explicit choice.
  useEffect(() => {
    const mq = window.matchMedia("(prefers-color-scheme: light)");
    const onChange = () => setSystem(mq.matches ? "light" : "dark");
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);

  const setTheme = useCallback((t: Theme) => {
    try {
      localStorage.setItem(STORAGE_KEY, t);
    } catch {
      /* storage disabled — theme still applies for this session */
    }
    setStored(t);
  }, []);

  const toggleTheme = useCallback(() => {
    setStored((prev) => {
      const next: Theme = (prev ?? systemTheme()) === "dark" ? "light" : "dark";
      try {
        localStorage.setItem(STORAGE_KEY, next);
      } catch {
        /* ignore */
      }
      return next;
    });
  }, []);

  const value = useMemo<ThemeContextValue>(
    () => ({ theme, isExplicit: stored != null, setTheme, toggleTheme }),
    [theme, stored, setTheme, toggleTheme],
  );

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}

export function useTheme(): ThemeContextValue {
  const ctx = useContext(ThemeContext);
  if (!ctx) throw new Error("useTheme must be used within <ThemeProvider>");
  return ctx;
}

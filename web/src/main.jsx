import React from "react";
import { createRoot } from "react-dom/client";
import { App } from "./app.jsx";
import { ConfirmationProvider } from "./components/ui.jsx";

const appRoot = globalThis.__chatgptOnboardingRoot || createRoot(document.getElementById("root"));
globalThis.__chatgptOnboardingRoot = appRoot;
appRoot.render(<ConfirmationProvider><App /></ConfirmationProvider>);

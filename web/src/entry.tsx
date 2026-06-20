/* @refresh reload */
import { render } from "solid-js/web";
import { Router } from "@solidjs/router";
import App from "./app";
import { AuthProvider } from "./context/auth";

const root = document.getElementById("root");
if (!root) throw new Error("root element missing");

render(
  () => (
    <AuthProvider>
      <Router>
        <App />
      </Router>
    </AuthProvider>
  ),
  root,
);

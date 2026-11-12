# Contributing

1. Open an issue first for anything larger than a small fix.
2. Build and test: `make check`. See [docs/development.md](docs/development.md).
3. Keep changes small and say in the pull request what you tested and on which
   distribution.

UI rules:

- No animation, low resource use.
- Text is never truncated with an ellipsis; layouts must fit.
- Status is a coloured dot with the word Ok, Warning or Error. No coloured text.
- Use the themed `Select`, `ComboInput` and `Menu` components, not native ones.

Contributions are accepted under the [Apache License 2.0](LICENSE), the
project's licence.
